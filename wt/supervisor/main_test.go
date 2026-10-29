package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestLogRingKeepsTail(t *testing.T) {
	r := newLogRing()
	if _, err := r.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if got := r.String(); got != "first" {
		t.Errorf("String() = %q, want %q", got, "first")
	}
	big := make([]byte, maxLaunchLogBytes+100)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := r.Write(big); err != nil {
		t.Fatal(err)
	}
	if got := len(r.String()); got != maxLaunchLogBytes {
		t.Errorf("len(String()) = %d, want %d", got, maxLaunchLogBytes)
	}
}

func TestWithLaunchLog(t *testing.T) {
	base := errors.New("wasmtime exited")
	if err := withLaunchLog("main.cwasm", newLogRing(), base); err != base {
		t.Errorf("empty ring should return the error unchanged, got %v", err)
	}
	recent := newLogRing()
	if _, err := recent.Write([]byte("boom\n")); err != nil {
		t.Fatal(err)
	}
	err := withLaunchLog("/tmp/x/main.cwasm", recent, base)
	if !errors.Is(err, base) {
		t.Errorf("should wrap the base error, got %v", err)
	}
	for _, want := range []string{"main.cwasm output", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
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

func TestHandleProxyLogsRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
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

	s := testSupervisor()
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{name: "test", cfg: appConfig{Host: "app.example"}, logs: logs}
	a.proxy = s.newProxy(&app{name: "test", port: port})
	s.apps["test"] = a
	s.hosts["app.example"] = a

	for _, path := range []string{"/hello?x=1", "/missing"} {
		req := httptest.NewRequest(http.MethodGet, "http://app.example"+path, nil)
		req.Host = "app.example"
		rec := httptest.NewRecorder()
		s.handleProxy(rec, req)
	}

	entries, err := logs.tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("logged %d requests, want 2: %+v", len(entries), entries)
	}
	if entries[0].Level != "info" || !strings.HasPrefix(entries[0].Message, "GET /hello?x=1 200 ") {
		t.Errorf("first entry = %+v, want an info access line for GET /hello?x=1 200", entries[0])
	}
	if entries[1].Level != "info" || !strings.HasPrefix(entries[1].Message, "GET /missing 404 ") {
		t.Errorf("second entry = %+v, want an info access line for GET /missing 404", entries[1])
	}
}

func TestFormatProxyDuration(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{500 * time.Microsecond, "500µs"},
		{1500 * time.Microsecond, "1.5ms"},
		{2500 * time.Millisecond, "2.50s"},
	}
	for _, tc := range cases {
		if got := formatProxyDuration(tc.elapsed); got != tc.want {
			t.Errorf("formatProxyDuration(%s) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}
