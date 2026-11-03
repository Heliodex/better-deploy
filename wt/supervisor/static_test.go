package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func staticTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"static/index.html":               "<html>spa</html>",
		"static/_app/immutable/app.js":    "console.log('app')",
		"static/assets/logo.svg":          "<svg></svg>",
		"static/auth/callback/index.html": "<html>callback</html>",
		"static/about.html":               "<html>about</html>",
		"static/admin/page.html":          "<html>admin</html>",
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
		{"/auth/callback/", "text/html", "<html>callback</html>", "no-cache", true},
		{"/auth/callback/", "*/*", "<html>callback</html>", "no-cache", true},
		{"/about", "text/html", "<html>about</html>", "no-cache", true},
		{"/about/", "text/html", "<html>about</html>", "no-cache", true},
		{"/about", "*/*", "<html>about</html>", "no-cache", true},
		{"/about.html", "text/html", "<html>about</html>", "no-cache", true},
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

func TestServeStaticRespectsServerPaths(t *testing.T) {
	a := staticTestApp(t)
	a.cfg.ServerPaths = []string{"/auth", "/admin"}

	cases := []struct {
		path, accept, body string
		served             bool
	}{
		{"/auth/login", "text/html", "", false},
		{"/auth/login", "*/*", "", false},
		{"/auth/callback/", "text/html", "<html>callback</html>", true},
		{"/admin/page", "text/html", "<html>admin</html>", true},
		{"/admin/page/", "text/html", "<html>admin</html>", true},
		{"/admin/missing", "text/html", "", false},
		{"/assets/logo.svg", "image/svg+xml", "<svg></svg>", true},
		{"/authentic", "text/html", "<html>spa</html>", true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://demo.localhost"+tc.path, nil)
		req.Header.Set("Accept", tc.accept)

		rec := httptest.NewRecorder()
		if got := a.serveStatic(rec, req); got != tc.served {
			t.Errorf("%s served = %v, want %v", tc.path, got, tc.served)
			continue
		}

		if tc.served && rec.Body.String() != tc.body {
			t.Errorf("%s body = %q, want %q", tc.path, rec.Body.String(), tc.body)
		}
	}

	a.cfg.ServerPaths = []string{"/"}
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/missing", nil)
	req.Header.Set("Accept", "text/html")

	if a.serveStatic(httptest.NewRecorder(), req) {
		t.Error("root server path still fell back to index.html")
	}
}

func TestServerPathMatch(t *testing.T) {
	prefixes := []string{"/auth", "/api/v1"}

	for path, want := range map[string]bool{
		"/auth":         true,
		"/auth/":        true,
		"/auth/login":   true,
		"/authentic":    false,
		"/api/v1":       true,
		"/api/v1/users": true,
		"/api/v2":       false,
	} {
		if got := serverPathMatch(path, prefixes); got != want {
			t.Errorf("serverPathMatch(%q) = %v, want %v", path, got, want)
		}
	}

	if !serverPathMatch("/anything", []string{"/"}) {
		t.Error("root prefix did not match")
	}
}

func TestStoppedAppDoesNotServeStatic(t *testing.T) {
	s := testSupervisor()
	a := staticTestApp(t)
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a

	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	req.Header.Set("Accept", "text/html")

	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("stopped app status = %d, want 503", rec.Code)
	}

	if strings.Contains(rec.Body.String(), "spa") {
		t.Error("stopped app served its static index")
	}

	a.mu.Lock()
	a.cmd = &exec.Cmd{}
	a.ready = true
	a.mu.Unlock()

	rec = httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "spa") {
		t.Errorf("running app static response = %d %q", rec.Code, rec.Body.String())
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

func TestServePrecompressedStatic(t *testing.T) {
	a := staticTestApp(t)
	dir := a.staticDir()

	files := map[string]string{
		"_app/immutable/app.js.br": "br-app",
		"_app/immutable/app.js.gz": "gz-app",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct{ accept, body, encoding string }{
		{"br, gzip", "br-app", "br"},
		{"gzip", "gz-app", "gzip"},
		{"gzip;q=0, br;q=1", "br-app", "br"},
		{"identity", "console.log('app')", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/_app/immutable/app.js", nil)
		req.Header.Set("Accept-Encoding", test.accept)

		rec := httptest.NewRecorder()
		if !a.serveStatic(rec, req) {
			t.Fatalf("%s was not served", test.accept)
		}

		if rec.Body.String() != test.body || rec.Header().Get("Content-Encoding") != test.encoding {
			t.Errorf("%s = encoding %q body %q, want %q %q", test.accept, rec.Header().Get("Content-Encoding"), rec.Body.String(), test.encoding, test.body)
		}

		if rec.Header().Get("Content-Type") == "" {
			t.Errorf("%s has no content type", test.accept)
		}

		if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s Vary = %q", test.accept, rec.Header().Get("Vary"))
		}
	}
}
