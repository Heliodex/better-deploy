package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testBlobSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBlobSignatureVector(t *testing.T) {
	t.Setenv("WT_APP_NAME", "demo")
	digest := strings.Repeat("a", 64)
	if got := blobSignature(testBlobSecret, "download", digest, 1700000000, 0, ""); got != "60761d4c71d09d7761f861bf9d685efbda078603748f5d5de4fa2fd6516ffce4" {
		t.Errorf("download signature = %s", got)
	}
	if got := blobSignature(testBlobSecret, "upload", "", 1700000000, 1024, "text/plain"); got != "34961813da763a9ce6d1b3e40dbd60e59a6db5ec2f094b63a99f61ab6a1e0153" {
		t.Errorf("upload signature = %s", got)
	}
}

func TestDeleteBlob(t *testing.T) {
	var gotPath, gotAuth string
	status := http.StatusNoContent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer server.Close()
	t.Setenv(storageURLEnv, server.URL)
	t.Setenv(storageTokenEnv, "secret")
	digest := strings.Repeat("a", 64)

	if err := DeleteBlob(t.Context(), server.Client(), digest); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/blobs/"+digest || gotAuth != "Bearer secret" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
	status = http.StatusNotFound
	if err := DeleteBlob(t.Context(), server.Client(), digest); err != nil {
		t.Errorf("missing blob should be idempotent: %v", err)
	}
	status = http.StatusInternalServerError
	if err := DeleteBlob(t.Context(), server.Client(), digest); err == nil {
		t.Error("server error was ignored")
	}
}

func TestDeleteBlobRejectsInvalidDigest(t *testing.T) {
	if err := DeleteBlob(t.Context(), http.DefaultClient, "nope"); err == nil {
		t.Error("invalid digest was accepted")
	}
}

func TestBlobDownloadURL(t *testing.T) {
	t.Setenv(blobSecretEnv, testBlobSecret)
	t.Setenv("WT_APP_NAME", "demo")
	t.Setenv(blobPrefixEnv, "")

	digest := strings.Repeat("a", 64)
	link, err := BlobDownloadURL(digest, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != BlobPrefix+"/"+digest {
		t.Errorf("path = %q", parsed.Path)
	}
	expires, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := blobSignature(testBlobSecret, "download", digest, expires, 0, "")
	if parsed.Query().Get("sig") != want {
		t.Errorf("signature = %q, want %q", parsed.Query().Get("sig"), want)
	}
}

func TestBlobUploadURL(t *testing.T) {
	t.Setenv(blobSecretEnv, testBlobSecret)
	t.Setenv("WT_APP_NAME", "demo")
	t.Setenv(blobPrefixEnv, "")

	link, err := BlobUploadURL(1024, "text/plain", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != BlobPrefix {
		t.Errorf("path = %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("max") != "1024" || query.Get("ct") != "text/plain" || query.Get("exp") == "" || query.Get("sig") == "" {
		t.Errorf("query = %v", query)
	}
	expires, err := strconv.ParseInt(query.Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if want := blobSignature(testBlobSecret, "upload", "", expires, 1024, "text/plain"); query.Get("sig") != want {
		t.Errorf("signature = %q, want %q", query.Get("sig"), want)
	}
}

func TestBlobURLRequiresSecret(t *testing.T) {
	t.Setenv(blobSecretEnv, "")
	if _, err := BlobDownloadURL(strings.Repeat("a", 64), time.Now().Add(time.Hour)); err == nil {
		t.Error("BlobDownloadURL accepted an empty secret")
	}
	if _, err := BlobUploadURL(10, "text/plain", time.Hour); err == nil {
		t.Error("BlobUploadURL accepted an empty secret")
	}
	if _, err := BlobUploadURL(10, "text/plain", 48*time.Hour); err == nil {
		t.Error("BlobUploadURL accepted a 48h lifetime")
	}
}
