package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAppURLUsesPerAppAndBaseEnv(t *testing.T) {
	t.Setenv("WT_APP_URL_NOTES", "http://127.0.0.1:9001/apps/notes/")
	t.Setenv("WT_APP_FETCH_URL", "http://127.0.0.1:9000")
	if got := AppURL("notes"); got != "http://127.0.0.1:9001/apps/notes" {
		t.Errorf("known app URL = %q", got)
	}
	if got := AppURL("later-app"); got != "http://127.0.0.1:9000/apps/later-app" {
		t.Errorf("fallback app URL = %q", got)
	}
}

func TestNewAppRequestAddsIdentity(t *testing.T) {
	t.Setenv("WT_APP_URL_NOTES", "http://127.0.0.1:9001/apps/notes")
	t.Setenv("WT_APP_TOKEN", "caller-token")
	req, err := NewAppRequest(t.Context(), http.MethodPost, "notes", "/v1/items", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "http://127.0.0.1:9001/apps/notes/v1/items" {
		t.Errorf("URL = %q", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer caller-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestCallerVerifiesSupervisorSignature(t *testing.T) {
	t.Setenv("WT_APP_TOKEN", "target-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/items?limit=1", nil)
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	req.Header.Set(AppCallerHeader, "frontend")
	req.Header.Set(AppTimestampHeader, timestamp)
	req.Header.Set(AppSignatureHeader, appCallSignature("target-token", "frontend", http.MethodGet, "/v1/items?limit=1", timestamp))

	caller, ok := Caller(req)
	if !ok || caller != "frontend" {
		t.Fatalf("Caller = %q, %v", caller, ok)
	}

	req.URL.Path = "/v1/other"
	req.URL.RawQuery = ""
	if caller, ok := Caller(req); ok {
		t.Errorf("tampered path verified as %q", caller)
	}
}

func TestCallerRejectsStaleAndMissingHeaders(t *testing.T) {
	t.Setenv("WT_APP_TOKEN", "target-token")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := Caller(req); ok {
		t.Error("missing headers verified")
	}

	timestamp := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	req.Header.Set(AppCallerHeader, "frontend")
	req.Header.Set(AppTimestampHeader, timestamp)
	req.Header.Set(AppSignatureHeader, appCallSignature("target-token", "frontend", http.MethodGet, "/", timestamp))
	if _, ok := Caller(req); ok {
		t.Error("stale signature verified")
	}
}
