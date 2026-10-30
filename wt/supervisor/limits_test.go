package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func backendApp(t *testing.T, s *supervisor, handler http.Handler, cfg appConfig) *app {
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
	a := &app{name: "demo", port: port, cfg: cfg}
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
}
