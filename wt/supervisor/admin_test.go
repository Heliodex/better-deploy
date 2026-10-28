package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSupervisor() *supervisor {
	return &supervisor{
		cfg:   config{token: "secret"},
		apps:  map[string]*app{},
		hosts: map[string]*app{},
	}
}

func TestHandleAppsRequiresAuth(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	rec := httptest.NewRecorder()
	s.handleApps(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApps(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleUnknownApp(t *testing.T) {
	s := testSupervisor()
	for _, target := range []string{"/apps/nope", "/apps/nope/logs", "/apps/nope/restart"} {
		method := http.MethodGet
		if strings.HasSuffix(target, "restart") {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestHandleAppMethods(t *testing.T) {
	s := testSupervisor()
	cases := []struct {
		method, target, allow string
	}{
		{http.MethodDelete, "/apps/nope", "GET, POST"},
		{http.MethodPost, "/apps/nope/logs", http.MethodGet},
		{http.MethodGet, "/apps/nope/restart", http.MethodPost},
		{http.MethodPut, "/apps/nope/unknown", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		if tc.allow == "" {
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.target, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.target, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.target, got, tc.allow)
		}
	}
}

func TestHandleLogsValidation(t *testing.T) {
	s := testSupervisor()
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{name: "test", logs: logs}

	for _, query := range []string{"?tail=0", "?tail=-1", "?tail=abc", "?tail=99999"} {
		req := httptest.NewRequest(http.MethodGet, "/apps/test/logs"+query, nil)
		rec := httptest.NewRecorder()
		s.handleLogs(rec, req, a)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tail %s = %d, want 400", query, rec.Code)
		}
	}
}

func TestAppLogRotation(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 100) + "\n"
	for i := 0; i < maxLogBytes/100+100; i++ {
		if _, err := fmt.Fprintf(logs, "line %d %s", i, line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(logs.dir, "output.log.1")); err != nil {
		t.Errorf("expected rotated output.log.1: %v", err)
	}

	got, err := logs.tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("tail(5) returned %d lines", len(got))
	}
	for _, line := range got {
		if !strings.HasPrefix(line, "line ") {
			t.Errorf("unexpected tail line %q", line)
		}
	}

	got, err = logs.tail(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("tail returned no lines")
	}
}
