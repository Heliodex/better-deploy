package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain doubles as a fake wasmtime: tests launch the test binary itself, and it serves health checks instead of running tests.
func TestMain(m *testing.M) {
	for i, arg := range os.Args {
		if arg != "--addr" || i+1 >= len(os.Args) {
			continue
		}
		err := http.ListenAndServe(os.Args[i+1], http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

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
	if entries[0].Level != "info" || entries[0].Source != "supervisor" || !strings.HasPrefix(entries[0].Message, "GET 200 ") || !strings.HasSuffix(entries[0].Message, " /hello?x=1") {
		t.Errorf("first entry = %+v, want a structured supervisor access line for GET 200 /hello?x=1", entries[0])
	}
	if entries[1].Level != "info" || entries[1].Source != "supervisor" || !strings.HasPrefix(entries[1].Message, "GET 404 ") || !strings.HasSuffix(entries[1].Message, " /missing") {
		t.Errorf("second entry = %+v, want a structured supervisor access line for GET 404 /missing", entries[1])
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

func TestFormatAccessLog(t *testing.T) {
	cases := []struct {
		method    string
		status    int
		elapsed   time.Duration
		uri, want string
	}{
		{"GET", 200, 1500 * time.Microsecond, "/hello?x=1", "GET 200 1.5ms /hello?x=1"},
		{"POST", 201, 2500 * time.Millisecond, "/notes", "POST 201 2.50s /notes"},
		{"GET", 404, 719 * time.Microsecond, "/missing", "GET 404 719µs /missing"},
	}
	for _, tc := range cases {
		if got := formatAccessLog(tc.method, tc.status, tc.elapsed, tc.uri); got != tc.want {
			t.Errorf("formatAccessLog(%s, %d, %s, %s) = %q, want %q", tc.method, tc.status, tc.elapsed, tc.uri, got, tc.want)
		}
	}
}

func TestWaitReadyRequiresSuccessStatus(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "still migrating", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := &supervisor{}
	a := &app{port: port, cfg: appConfig{Health: "/"}}
	if err := s.waitReady(a, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("waitReady accepted the 500 after %d call(s)", got)
	}
}

func TestHandleRestartRestartsSchedule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	s.cfg.wasmtime = os.Args[0]
	s.cfg.stateDir = t.TempDir()
	a, err := s.spawn("demo", dir, appConfig{
		Health: "/",
		Host:   "demo.localhost",
		Name:   "demo",
		Schedule: []scheduleConfig{
			{Path: "/tick", Every: "1s"},
		},
	}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop(a)
	s.register(a)
	if a.scheduleCancel == nil {
		t.Fatal("spawn should start the schedule")
	}

	rec := httptest.NewRecorder()
	s.handleRestart(rec, httptest.NewRequest(http.MethodPost, "/apps/demo/restart", nil), a)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart status = %d: %s", rec.Code, rec.Body.String())
	}
	if a.scheduleCancel == nil {
		t.Error("handleRestart did not restart the schedule")
	}
}

func TestDrainApp(t *testing.T) {
	a := &app{name: "demo"}
	a.requests.Add(1)
	if drainApp(a, 20*time.Millisecond) {
		t.Error("drain finished while a request was active")
	}
	a.requests.Done()
	if !drainApp(a, time.Second) {
		t.Error("drain did not finish after the request ended")
	}
}

func TestHandleProxyTracksInFlightRequests(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(received)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	a := &app{name: "demo", port: port, cfg: appConfig{Host: "demo.localhost"}}
	a.proxy = s.newProxy(a)
	s.hosts["demo.localhost"] = a

	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "demo.localhost"
		s.handleProxy(httptest.NewRecorder(), req)
		close(done)
	}()
	<-received
	if drainApp(a, 20*time.Millisecond) {
		t.Error("drain finished while a proxied request was in flight")
	}
	close(release)
	<-done
	if !drainApp(a, time.Second) {
		t.Error("drain did not finish after the proxied request completed")
	}
}

func TestHandlerLetsAppHostsOwnAdminPaths(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("app " + r.URL.Path))
	}))
	defer backend.Close()

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	a := &app{name: "demo", port: port, cfg: appConfig{Host: "demo.localhost"}}
	a.proxy = s.newProxy(a)
	s.hosts["demo.localhost"] = a
	handler := s.handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/admin/", nil)
	req.Host = "demo.localhost"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "app /admin/" {
		t.Errorf("app host admin path = %d %q, want 200 \"app /admin/\"", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://admin.localhost/healthz", nil)
	req.Host = "admin.localhost"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("non-app healthz = %d %q, want 200 \"ok\\n\"", rec.Code, rec.Body.String())
	}
}

func TestLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":   true,
		"localhost:8080":   true,
		"[::1]:8080":       true,
		"0.0.0.0:8080":     false,
		":8080":            false,
		"example.com:8080": false,
		"not-an-address":   false,
	}
	for addr, want := range cases {
		if got := loopbackListen(addr); got != want {
			t.Errorf("loopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}
