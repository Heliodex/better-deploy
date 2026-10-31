package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func backendApp(t testing.TB, s *supervisor, handler http.Handler, cfg appConfig) *app {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{name: "demo", port: port, cfg: cfg, cmd: &exec.Cmd{}, healthOK: true, ready: true}
	if cfg.MaxConcurrent > 0 {
		a.slots = make(chan struct{}, cfg.MaxConcurrent)
	}
	a.proxy = s.newProxy(a)
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a
	return a
}

func TestProxyConcurrencyLimit(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	s := testSupervisor()
	backendApp(t, s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { entered <- struct{}{} })
		<-release
		_, _ = w.Write([]byte("ok"))
	}), appConfig{MaxConcurrent: 1})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
		firstDone <- rec
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the app")
	}

	second := httptest.NewRecorder()
	s.handleProxy(second, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("second concurrent request = %d, want 429", second.Code)
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Errorf("first request = %d, want 200", first.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
}

func TestProxyRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := testSupervisor()
	backendApp(t, s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}), appConfig{requestTimeout: 50 * time.Millisecond})

	rec := httptest.NewRecorder()
	started := time.Now()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 after the request deadline", rec.Code)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("request waited %s for a hung app", elapsed)
	}
}

func TestNormalizeRequestLimits(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{RequestTimeout: "30s", MaxConcurrent: 4}, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.requestTimeout != 30*time.Second || cfg.MaxConcurrent != 4 || cfg.Network != "inherit" {
		t.Errorf("normalized = %+v", cfg)
	}

	for _, bad := range []appConfig{
		{RequestTimeout: "soon"},
		{RequestTimeout: "100ms"},
		{RequestTimeout: "48h"},
		{MaxConcurrent: -1},
		{Network: "open"},
	} {
		if _, err := normalizeAppConfig(bad, "demo", "localhost"); err == nil {
			t.Errorf("config %+v should be rejected", bad)
		}
	}
}

func TestNormalizeHealthSettings(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{}, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health != defaultHealthPath {
		t.Errorf("default health = %q, want %q", cfg.Health, defaultHealthPath)
	}
	if cfg.startupTimeout != defaultStartupTimeout || cfg.healthInterval != defaultHealthInterval || cfg.HealthFailures != defaultHealthFailures {
		t.Errorf("default health settings = %+v", cfg)
	}

	cfg, err = normalizeAppConfig(appConfig{
		StartupTimeout:     "30s",
		HealthInterval:     "5s",
		HealthFailures:     3,
		RestartOnUnhealthy: true,
	}, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.startupTimeout != 30*time.Second || cfg.healthInterval != 5*time.Second {
		t.Errorf("parsed health settings = %+v", cfg)
	}
	if cfg.HealthFailures != 3 || !cfg.RestartOnUnhealthy {
		t.Errorf("health failure settings = %+v", cfg)
	}

	for _, bad := range []appConfig{
		{StartupTimeout: "soon"},
		{StartupTimeout: "100ms"},
		{HealthInterval: "100ms"},
		{HealthInterval: "2h"},
		{HealthFailures: -1},
		{HealthFailures: 101},
	} {
		if _, err := normalizeAppConfig(bad, "demo", "localhost"); err == nil {
			t.Errorf("config %+v should be rejected", bad)
		}
	}
}

func TestAppArgsNetwork(t *testing.T) {
	s := testSupervisor()
	a := &app{name: "demo", port: 1, cfg: appConfig{Network: "inherit"}}
	if args := strings.Join(s.appArgs(a, "main.cwasm"), " "); !strings.Contains(args, "-S inherit-network") {
		t.Errorf("inherit args = %q", args)
	}
	a.cfg.Network = "none"
	if args := strings.Join(s.appArgs(a, "main.cwasm"), " "); strings.Contains(args, "inherit-network") {
		t.Errorf("isolated args = %q", args)
	}

	a.cfg.requestTimeout = 30 * time.Second
	a.wasmEpoch = true
	args := strings.Join(s.appArgs(a, "main.cwasm"), " ")
	for _, want := range []string{"epoch-interruption=y", "timeout=30s"} {
		if !strings.Contains(args, want) {
			t.Errorf("timeout args = %q, missing %q", args, want)
		}
	}
	a.wasmEpoch = false
	args = strings.Join(s.appArgs(a, "main.cwasm"), " ")
	if strings.Contains(args, "timeout=") || strings.Contains(args, "epoch-interruption") {
		t.Errorf("artifact without epoch got epoch args = %q", args)
	}
}

func BenchmarkProxyLatency(b *testing.B) {
	s := testSupervisor()
	backendApp(b, s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), appConfig{})
	proxy := s.apps["demo"].proxy
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
