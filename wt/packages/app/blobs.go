package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// BlobPrefix is the reserved same-origin path the supervisor serves blobs from.
	BlobPrefix    = "/_wt/blobs"
	blobSecretEnv = "WT_BLOB_SECRET"
	blobPrefixEnv = "WT_BLOB_PREFIX"
	blobMaxTTL    = 24 * time.Hour
)

// blobSecret reads and validates the 64-character hex key shared with the supervisor.
func blobSecret() (string, error) {
	secret := os.Getenv(blobSecretEnv)
	if secret == "" {
		return "", fmt.Errorf("%s is not set", blobSecretEnv)
	}
	if len(secret) != sha256.Size*2 {
		return "", fmt.Errorf("%s must be a 64-character hex key", blobSecretEnv)
	}
	return secret, nil
}

// blobBase returns the configured same-origin blob prefix, defaulting to BlobPrefix.
func blobBase() string {
	if prefix := os.Getenv(blobPrefixEnv); prefix != "" {
		return prefix
	}
	return BlobPrefix
}

// blobSignature must stay byte-for-byte compatible with the supervisor.
func blobSignature(secret, method, digest string, expires, maxBytes int64, contentType string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%d\n%d\n%s", method, os.Getenv("WT_APP_NAME"), digest, expires, maxBytes, contentType)
	return hex.EncodeToString(mac.Sum(nil))
}

// blobExpiry validates a URL lifetime against the one-day maximum and returns the expiry time.
func blobExpiry(ttl time.Duration) (time.Time, error) {
	if ttl <= 0 || ttl > blobMaxTTL {
		return time.Time{}, fmt.Errorf("blob URL lifetime must be between 1s and %s", blobMaxTTL)
	}
	return time.Now().Add(ttl), nil
}

// BlobUploadURL returns a same-origin URL a browser can PUT the file body to.
func BlobUploadURL(maxBytes int64, contentType string, ttl time.Duration) (string, error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("blob max bytes must be positive")
	}
	secret, err := blobSecret()
	if err != nil {
		return "", err
	}
	expires, err := blobExpiry(ttl)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("exp", strconv.FormatInt(expires.Unix(), 10))
	query.Set("max", strconv.FormatInt(maxBytes, 10))
	query.Set("ct", contentType)
	query.Set("sig", blobSignature(secret, "upload", "", expires.Unix(), maxBytes, contentType))
	return blobBase() + "?" + query.Encode(), nil
}

// BlobDownloadURL returns a signed URL for one stored blob. expires must be within 24 hours.
func BlobDownloadURL(digest string, expires time.Time) (string, error) {
	secret, err := blobSecret()
	if err != nil {
		return "", err
	}
	if digest == "" {
		return "", fmt.Errorf("blob digest is empty")
	}
	now := time.Now()
	if !expires.After(now) || expires.After(now.Add(blobMaxTTL)) {
		return "", fmt.Errorf("blob URL expiry must be within %s", blobMaxTTL)
	}
	signature := blobSignature(secret, "download", digest, expires.Unix(), 0, "")
	return blobBase() + "/" + digest + "?exp=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + signature, nil
}

// BlobDeleteURL returns a signed URL for deleting one stored blob.
func BlobDeleteURL(digest string, expires time.Time) (string, error) {
	secret, err := blobSecret()
	if err != nil {
		return "", err
	}
	if digest == "" {
		return "", fmt.Errorf("blob digest is empty")
	}
	now := time.Now()
	if !expires.After(now) || expires.After(now.Add(blobMaxTTL)) {
		return "", fmt.Errorf("blob URL expiry must be within %s", blobMaxTTL)
	}
	signature := blobSignature(secret, "delete", digest, expires.Unix(), 0, "")
	return blobBase() + "/" + digest + "?exp=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + signature, nil
}

// DeleteBlob removes one of the app's stored blobs through the storage loopback service. A missing blob is treated as already deleted.
func DeleteBlob(ctx context.Context, client *http.Client, digest string) error {
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("blob digest must be 64 hex characters")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("blob digest must be 64 hex characters")
	}
	base := strings.TrimSuffix(os.Getenv(storageURLEnv), "/")
	token := os.Getenv(storageTokenEnv)
	if base == "" || token == "" {
		return fmt.Errorf("%s and %s are not set", storageURLEnv, storageTokenEnv)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/v1/blobs/"+digest, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete blob returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
