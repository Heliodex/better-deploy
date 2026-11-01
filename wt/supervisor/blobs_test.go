package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testBlobSupervisor(t *testing.T) (*supervisor, *app) {
	t.Helper()
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	store, err := newBlobStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = store
	a := &app{name: "demo", cfg: appConfig{Host: "demo.localhost", DiskMB: 1}}
	s.apps["demo"] = a
	s.hosts["demo.localhost"] = a
	return s, a
}

func signedBlobURL(t *testing.T, s *supervisor, a *app, method, digest string, maxBytes int64, contentType string, expires time.Time) string {
	t.Helper()
	signature := blobSignature(s.blobs.secretFor(a.name), method, a.name, digest, expires.Unix(), maxBytes, contentType)
	query := "exp=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + signature
	switch method {
	case "upload":
		return blobPrefix + "?max=" + strconv.FormatInt(maxBytes, 10) + "&ct=" + contentType + "&" + query
	default:
		return blobPrefix + "/" + digest + "?" + query
	}
}

func TestBlobSignatureVector(t *testing.T) {
	digest := strings.Repeat("a", 64)
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := blobSignature(secret, "download", "demo", digest, 1700000000, 0, ""); got != "60761d4c71d09d7761f861bf9d685efbda078603748f5d5de4fa2fd6516ffce4" {
		t.Errorf("download signature = %s", got)
	}
	if got := blobSignature(secret, "upload", "demo", "", 1700000000, 1024, "text/plain"); got != "34961813da763a9ce6d1b3e40dbd60e59a6db5ec2f094b63a99f61ab6a1e0153" {
		t.Errorf("upload signature = %s", got)
	}
}

func TestBlobUploadDownloadAndDelete(t *testing.T) {
	s, a := testBlobSupervisor(t)
	expires := time.Now().Add(time.Hour)
	body := []byte("hello blob")
	uploadURL := signedBlobURL(t, s, a, "upload", "", 1024, "text/plain", expires)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(body))
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var uploaded blobUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if uploaded.Digest != digest || uploaded.Size != int64(len(body)) || uploaded.ContentType != "text/plain" {
		t.Errorf("uploaded = %+v", uploaded)
	}

	downloadURL := signedBlobURL(t, s, a, "download", digest, 0, "", expires)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, downloadURL, nil)
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"`+digest+`"` {
		t.Errorf("ETag = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, downloadURL, nil)
	req.Header.Set("Range", "bytes=2-5")
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != string(body[2:6]) {
		t.Errorf("range download = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") == "" {
		t.Error("range response has no Content-Range")
	}

	deleteURL := signedBlobURL(t, s, a, "delete", digest, 0, "", expires)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, deleteURL, nil)
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, downloadURL, nil)
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusNotFound {
		t.Errorf("download after delete = %d, want 404", rec.Code)
	}
}

func TestBlobRejectsBadSignatureAndOversize(t *testing.T) {
	s, a := testBlobSupervisor(t)
	expires := time.Now().Add(time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, blobPrefix+"?max=10&ct=text/plain&exp="+strconv.FormatInt(expires.Unix(), 10)+"&sig="+strings.Repeat("0", 64), strings.NewReader("hello"))
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bad signature = %d, want 403", rec.Code)
	}

	uploadURL := signedBlobURL(t, s, a, "upload", "", 4, "text/plain", expires)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, uploadURL, strings.NewReader("hello"))
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload = %d, want 413: %s", rec.Code, rec.Body.String())
	}
}

func TestBlobUploadRejectsOverQuota(t *testing.T) {
	s, a := testBlobSupervisor(t)
	dataDir := filepath.Join(s.cfg.stateDir, "demo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "large"), make([]byte, 900<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	uploadURL := signedBlobURL(t, s, a, "upload", "", 300<<10, "application/octet-stream", expires)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(make([]byte, 300<<10)))
	s.handleBlobs(rec, req, a)
	if rec.Code != http.StatusInsufficientStorage {
		t.Errorf("over-quota upload = %d, want 507: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.cfg.stateDir, ".blobs", "demo")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(s.cfg.stateDir, ".blobs", "demo"))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				t.Errorf("over-quota upload left %s behind", entry.Name())
			}
		}
	}
}
