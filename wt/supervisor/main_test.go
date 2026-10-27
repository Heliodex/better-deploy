package main

import (
	"archive/tar"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "app-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"hello", "notes", "my-app", "a1"} {
		if !validName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "-app", "app-", "App", "app_name", "app.name", "../app"} {
		if validName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	archive := writeTarGz(t, map[string]string{"app.json": "{}", "main.wasm": "wasm"})
	dst := t.TempDir()
	if err := extractTarGz(archive, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.json", "main.wasm"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	archive := writeTarGz(t, map[string]string{"../evil": "nope"})
	if err := extractTarGz(archive, t.TempDir()); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestProxyRewrite(t *testing.T) {
	type request struct {
		host, forwardedHost, forwardedProto, forwardedFor, target string
	}
	got := make(chan request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- request{
			host:           r.Host,
			forwardedHost:  r.Header.Get("X-Forwarded-Host"),
			forwardedProto: r.Header.Get("X-Forwarded-Proto"),
			forwardedFor:   r.Header.Get("X-Forwarded-For"),
			target:         r.URL.RequestURI(),
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := &supervisor{}
	proxy := s.newProxy(&app{name: "test", port: port})
	req := httptest.NewRequest(http.MethodGet, "http://app.example/hello?x=1", nil)
	req.Host = "app.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	select {
	case request := <-got:
		if request.host != "app.example" {
			t.Errorf("host = %q", request.host)
		}
		if request.forwardedHost != "app.example" {
			t.Errorf("X-Forwarded-Host = %q", request.forwardedHost)
		}
		if request.forwardedProto != "https" {
			t.Errorf("X-Forwarded-Proto = %q", request.forwardedProto)
		}
		if request.forwardedFor != "192.0.2.1" {
			t.Errorf("X-Forwarded-For = %q", request.forwardedFor)
		}
		if request.target != "/hello?x=1" {
			t.Errorf("target = %q", request.target)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not receive the request")
	}
}
