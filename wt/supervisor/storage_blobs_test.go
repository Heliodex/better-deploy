package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageBlobDelete(t *testing.T) {
	s, _ := testBlobSupervisor(t)
	storage := &storageServer{blobs: s.blobs}

	info, _, err := s.blobs.store("demo", "text/plain", strings.NewReader("hello"), 16)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	storage.storageRoutes().serve(rec, httptest.NewRequest(http.MethodDelete, "/v1/blobs/"+info.Digest, nil), "demo")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(s.blobs.appDir("demo"), info.Digest)); !os.IsNotExist(err) {
		t.Errorf("blob still exists: %v", err)
	}

	rec = httptest.NewRecorder()
	storage.storageRoutes().serve(rec, httptest.NewRequest(http.MethodDelete, "/v1/blobs/"+info.Digest, nil), "demo")

	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}
