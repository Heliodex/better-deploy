// Per-app content-addressed blob store with signed upload, download and delete URLs.
// Blobs are served by the supervisor on the app host under /_wt/blobs, away from the wasm request path.

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	blobPrefix        = "/_wt/blobs"
	blobDirName       = ".blobs"
	blobSecretName    = ".blob-secret"
	blobMaxTTL        = 24 * time.Hour
	blobMaxUpload     = 256 << 20
	blobContentType   = 256
	blobSignatureSize = 32
	blobSecretEnv     = "WT_BLOB_SECRET"
	blobPrefixEnv     = "WT_BLOB_PREFIX"
)

var (
	errBlobTooLarge   = errors.New("blob is too large")
	errBlobSignature  = errors.New("invalid blob signature")
	errBlobExpired    = errors.New("blob URL has expired")
	errBlobInvalid    = errors.New("invalid blob request")
	errBlobNotFound   = errors.New("blob not found")
	errBlobQuota      = errors.New("blob quota exceeded")
	errBlobStoreEmpty = errors.New("blob store is unavailable")
)

type blobInfo struct {
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	CreatedAt   time.Time `json:"createdAt"`
}

type blobStore struct {
	dir    string
	secret []byte

	mu sync.Mutex
}

// newBlobStore loads or creates the supervisor-wide signing secret and prepares the blob root.
func newBlobStore(stateDir string) (*blobStore, error) {
	dir := filepath.Join(stateDir, blobDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blob dir: %w", err)
	}
	path := filepath.Join(stateDir, blobSecretName)
	secret, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("blob secret: %w", err)
		}
		if err := os.WriteFile(path, secret, 0o600); err != nil {
			return nil, fmt.Errorf("write blob secret: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read blob secret: %w", err)
	}
	if len(secret) != 32 {
		return nil, fmt.Errorf("blob secret must be 32 bytes, got %d", len(secret))
	}
	return &blobStore{dir: dir, secret: secret}, nil
}

func (b *blobStore) appDir(app string) string {
	return filepath.Join(b.dir, app)
}

// secretFor derives one app's signing key from the supervisor secret, so an app cannot sign for another app.
func (b *blobStore) secretFor(app string) string {
	mac := hmac.New(sha256.New, b.secret)
	_, _ = mac.Write([]byte(app))
	return hex.EncodeToString(mac.Sum(nil))
}

// validBlobDigest reports whether digest is a 64-character hex SHA-256.
func validBlobDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// blobSignature is the shared signing input for the supervisor and the Go app SDK.
func blobSignature(secret, method, app, digest string, expires, maxBytes int64, contentType string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%d\n%d\n%s", method, app, digest, expires, maxBytes, contentType)
	return hex.EncodeToString(mac.Sum(nil))
}

// sign derives the app's key and signs one blob action.
func (b *blobStore) sign(method, app, digest string, expires time.Time, maxBytes int64, contentType string) (string, error) {
	if b == nil {
		return "", errBlobStoreEmpty
	}
	return blobSignature(b.secretFor(app), method, app, digest, expires.Unix(), maxBytes, contentType), nil
}

// downloadURL returns a same-origin signed URL for one stored blob.
func (b *blobStore) downloadURL(app, digest string, expires time.Time) (string, error) {
	signature, err := b.sign("download", app, digest, expires, 0, "")
	if err != nil {
		return "", err
	}
	return blobPrefix + "/" + digest + "?exp=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + signature, nil
}

// store streams src into a temporary file, hashes it and moves it under its digest. It never trusts a client digest.
func (b *blobStore) store(app, contentType string, src io.Reader, maxBytes int64) (blobInfo, bool, error) {
	if b == nil {
		return blobInfo{}, false, errBlobStoreEmpty
	}
	if maxBytes <= 0 || maxBytes > blobMaxUpload {
		return blobInfo{}, false, errBlobInvalid
	}
	appDir := b.appDir(app)
	content := newContentStore(appDir, filepath.Join(appDir, ".tmp"))
	digest, written, created, tooLarge, err := content.putReader(src, maxBytes)
	if err != nil {
		return blobInfo{}, false, err
	}
	if tooLarge {
		return blobInfo{}, false, errBlobTooLarge
	}
	if !created {
		meta, _ := b.openMeta(app, digest)
		meta.Digest = digest
		meta.Size = written
		if meta.ContentType == "" {
			meta.ContentType = contentType
		}
		return meta, false, nil
	}
	info := blobInfo{Digest: digest, Size: written, ContentType: contentType, CreatedAt: time.Now().UTC()}
	if info.ContentType == "" {
		info.ContentType = "application/octet-stream"
	}
	data, err := json.Marshal(info)
	if err != nil {
		_ = os.Remove(content.path(digest))
		return blobInfo{}, false, err
	}
	if err := writeFileAtomicMode(filepath.Join(appDir, digest+".json"), append(data, '\n'), 0o600); err != nil {
		_ = os.Remove(content.path(digest))
		return blobInfo{}, false, err
	}
	return info, true, nil
}

func (b *blobStore) openMeta(app, digest string) (blobInfo, error) {
	data, err := os.ReadFile(filepath.Join(b.appDir(app), digest+".json"))
	if err != nil {
		return blobInfo{}, err
	}
	var info blobInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return blobInfo{}, err
	}
	return info, nil
}

// open returns the blob body and metadata, falling back to file details when the sidecar is missing.
func (b *blobStore) open(app, digest string) (*os.File, blobInfo, error) {
	if !validBlobDigest(digest) {
		return nil, blobInfo{}, errBlobNotFound
	}
	file, err := os.Open(filepath.Join(b.appDir(app), digest))
	if err != nil {
		return nil, blobInfo{}, errBlobNotFound
	}
	info, err := b.openMeta(app, digest)
	if err != nil {
		info = blobInfo{Digest: digest, ContentType: "application/octet-stream"}
		if stat, statErr := file.Stat(); statErr == nil {
			info.Size = stat.Size()
			info.CreatedAt = stat.ModTime().UTC()
		}
	}
	return file, info, nil
}

// remove deletes a blob and its metadata, reporting a missing blob as not found.
func (b *blobStore) remove(app, digest string) error {
	if !validBlobDigest(digest) {
		return errBlobNotFound
	}
	if err := os.Remove(filepath.Join(b.appDir(app), digest)); err != nil {
		if os.IsNotExist(err) {
			return errBlobNotFound
		}
		return err
	}
	_ = os.Remove(filepath.Join(b.appDir(app), digest+".json"))
	return nil
}

type blobUploadResponse struct {
	blobInfo
	URL string `json:"url"`
}

func (s *supervisor) handleBlobs(w http.ResponseWriter, r *http.Request, a *app) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, errBlobStoreEmpty.Error())
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, blobPrefix)
	if rest == "" || rest == "/" {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			w.Header().Set("Allow", "POST, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleBlobUpload(w, r, a)
		return
	}
	digest := strings.TrimPrefix(rest, "/")
	if strings.Contains(digest, "/") || !validBlobDigest(digest) {
		writeError(w, http.StatusNotFound, errBlobNotFound.Error())
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleBlobDownload(w, r, a, digest)
	case http.MethodDelete:
		s.handleBlobDelete(w, r, a, digest)
	default:
		w.Header().Set("Allow", "GET, HEAD, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// verifyBlobURL checks the expiry, action and HMAC signature of a signed blob URL.
func (s *supervisor) verifyBlobURL(r *http.Request, a *app, method, digest string, maxBytes int64, contentType string) error {
	value := r.URL.Query().Get("exp")
	expires, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errBlobInvalid
	}
	now := time.Now()
	if expires < now.Unix() {
		return errBlobExpired
	}
	if expires > now.Add(blobMaxTTL).Unix() {
		return errBlobInvalid
	}
	signature := r.URL.Query().Get("sig")
	if len(signature) != blobSignatureSize*2 {
		return errBlobSignature
	}
	want, err := hex.DecodeString(blobSignature(s.blobs.secretFor(a.name), method, a.name, digest, expires, maxBytes, contentType))
	if err != nil {
		return errBlobSignature
	}
	got, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(got, want) {
		return errBlobSignature
	}
	return nil
}

// handleBlobUpload stores a signed PUT body and returns a signed download URL.
func (s *supervisor) handleBlobUpload(w http.ResponseWriter, r *http.Request, a *app) {
	maxValue := r.URL.Query().Get("max")
	maxBytes, err := strconv.ParseInt(maxValue, 10, 64)
	if err != nil || maxBytes <= 0 || maxBytes > blobMaxUpload {
		writeError(w, http.StatusBadRequest, "max must be between 1 and "+strconv.Itoa(blobMaxUpload))
		return
	}
	contentType := r.URL.Query().Get("ct")
	if len(contentType) > blobContentType {
		writeError(w, http.StatusBadRequest, "content type is too long")
		return
	}
	if err := s.verifyBlobURL(r, a, "upload", "", maxBytes, contentType); err != nil {
		writeBlobError(w, err)
		return
	}
	if r.ContentLength > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errBlobTooLarge.Error())
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	info, created, err := s.blobs.store(a.name, contentType, r.Body, maxBytes)
	if err != nil {
		writeBlobError(w, err)
		return
	}
	if created {
		if s.blobOverQuota(a) {
			_ = s.blobs.remove(a.name, info.Digest)
			writeBlobError(w, errBlobQuota)
			return
		}
	}
	expires := time.Now().Add(time.Hour)
	url, err := s.blobs.downloadURL(a.name, info.Digest, expires)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, blobUploadResponse{blobInfo: info, URL: url})
}

// blobOverQuota reports whether an upload would exceed the app or host disk budget.
func (s *supervisor) blobOverQuota(a *app) bool {
	if s.cfg.diskBudgetBytes > 0 && s.overBudget() {
		return true
	}
	a.mu.Lock()
	limit := int64(a.cfg.DiskMB) << 20
	a.mu.Unlock()
	if limit <= 0 {
		return false
	}
	used, _, err := s.appDataUsage(a.name)
	if err != nil {
		return true
	}
	return used > limit
}

// handleBlobDownload serves a blob through a signed URL with an ETag and private caching.
func (s *supervisor) handleBlobDownload(w http.ResponseWriter, r *http.Request, a *app, digest string) {
	if err := s.verifyBlobURL(r, a, "download", digest, 0, ""); err != nil {
		writeBlobError(w, err)
		return
	}
	file, info, err := s.blobs.open(a.name, digest)
	if err != nil {
		writeBlobError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, digest, info.CreatedAt, file)
}

// handleBlobDelete removes a blob through a signed URL.
func (s *supervisor) handleBlobDelete(w http.ResponseWriter, r *http.Request, a *app, digest string) {
	if err := s.verifyBlobURL(r, a, "delete", digest, 0, ""); err != nil {
		writeBlobError(w, err)
		return
	}
	if err := s.blobs.remove(a.name, digest); err != nil {
		writeBlobError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeBlobError maps blob store errors to their HTTP statuses.
func writeBlobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBlobTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, errBlobExpired), errors.Is(err, errBlobSignature), errors.Is(err, errBlobInvalid):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, errBlobNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errBlobQuota):
		writeError(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, errBlobStoreEmpty):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
