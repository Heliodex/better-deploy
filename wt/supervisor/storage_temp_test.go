package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageRestoreToken(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	s.restoreToken("demo", "token-from-before-handover")

	if app, ok := s.appForToken("token-from-before-handover"); !ok || app != "demo" {
		t.Errorf("appForToken = %q, %t", app, ok)
	}

	if got := s.tokenFor("demo"); got != "token-from-before-handover" {
		t.Errorf("tokenFor = %q, want the restored token", got)
	}
}

func TestStorageTempBudgetRejectsRequests(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	dir := t.TempDir()
	if err := s.setTempDir(dir, 10); err != nil {
		t.Fatal(err)
	}

	blob := filepath.Join(dir, "blob")
	if err := os.WriteFile(blob, []byte("0123456789ABCDEF"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.checkTemp()

	if !s.tempOver.Load() {
		t.Fatal("temp usage did not exceed the budget")
	}

	token := s.tokenFor("app")
	request := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(`{"sql":"select 1"}`))
	request.Header.Set("Authorization", "Bearer "+token)

	recorder := httptest.NewRecorder()
	s.handle(recorder, request)

	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507: %s", recorder.Code, recorder.Body.String())
	}

	var body storageErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.Code != "temp_budget" {
		t.Errorf("code = %q, want temp_budget", body.Code)
	}

	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}

	s.checkTemp()

	if s.tempOver.Load() {
		t.Fatal("temp flag stayed set after usage fell below the budget")
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(`{"sql":"select 1"}`))
	request.Header.Set("Authorization", "Bearer "+token)

	recorder = httptest.NewRecorder()
	s.handle(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status after clearing = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSetTempDirCleansOnlySQLiteTemps(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	dir := t.TempDir()
	stale := filepath.Join(dir, "etilqs_stale")
	keep := filepath.Join(dir, "keep.txt")

	if err := os.WriteFile(stale, []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.setTempDir(dir, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale SQLite temp file was not removed: %v", err)
	}

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated file was removed: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o700 {
		t.Errorf("temp dir mode = %o, want 700", info.Mode().Perm())
	}
}

func TestValidateStorageTempDir(t *testing.T) {
	state := "/var/lib/wt"
	apps := "/var/lib/wt/apps"

	cases := []struct {
		tmp  string
		want bool
	}{
		{"/var/lib/wt/.tmp", true},
		{"/tmp/wt-tmp", true},
		{state, false},
		{apps, false},
		{"/var/lib", false},
		{"/var/lib/wt/../wt", false},
		{"/", false},
	}
	for _, tc := range cases {
		err := validateStorageTempDir(tc.tmp, state, apps)
		if (err == nil) != tc.want {
			t.Errorf("validateStorageTempDir(%q) = %v, want valid=%t", tc.tmp, err, tc.want)
		}
	}
}
