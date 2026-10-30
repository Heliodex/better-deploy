package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func staticTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"static/index.html":            "<html>spa</html>",
		"static/_app/immutable/app.js": "console.log('app')",
		"static/assets/logo.svg":       "<svg></svg>",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &app{name: "demo", dir: dir}
}

func TestServeStaticFiles(t *testing.T) {
	a := staticTestApp(t)

	cases := []struct {
		path, accept, body, cache string
		served                    bool
	}{
		{"/", "text/html", "<html>spa</html>", "no-cache", true},
		{"/_app/immutable/app.js", "*/*", "console.log('app')", "public, max-age=31536000, immutable", true},
		{"/assets/logo.svg", "image/svg+xml", "<svg></svg>", "no-cache", true},
		{"/missing", "text/html", "<html>spa</html>", "no-cache", true},
		{"/api/notes", "*/*", "", "", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://demo.localhost"+tc.path, nil)
		req.Header.Set("Accept", tc.accept)
		rec := httptest.NewRecorder()
		if got := a.serveStatic(rec, req); got != tc.served {
			t.Errorf("%s served = %v, want %v", tc.path, got, tc.served)
			continue
		}
		if !tc.served {
			continue
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d", tc.path, rec.Code)
		}
		if rec.Body.String() != tc.body {
			t.Errorf("%s body = %q, want %q", tc.path, rec.Body.String(), tc.body)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s cache-control = %q, want %q", tc.path, got, tc.cache)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "http://demo.localhost/", nil)
	req.Header.Set("Accept", "text/html")
	if a.serveStatic(httptest.NewRecorder(), req) {
		t.Error("POST was served from static files")
	}
}

func TestStaticFileRejectsTraversal(t *testing.T) {
	a := staticTestApp(t)
	dir := a.staticDir()
	for _, path := range []string{"/../main.cwasm", "/..%2fmain.cwasm", "/./../main.cwasm", "/"} {
		if _, ok := staticFile(dir, path); ok {
			t.Errorf("staticFile(%q) resolved outside the file set", path)
		}
	}
}
