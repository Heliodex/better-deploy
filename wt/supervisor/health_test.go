package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testApp(port int) *app {
	return &app{
		name:     "demo",
		port:     port,
		cfg:      appConfig{Health: "/"},
		cmd:      &exec.Cmd{},
		healthOK: true,
	}
}

func TestAppStatusLocked(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	cases := []struct {
		name string
		app  *app
		want string
	}{
		{"retired", &app{retired: true}, "stopped"},
		{"starting", &app{starting: true}, "restarting"},
		{"waiting to restart", &app{timer: timer}, "restarting"},
		{"healthy", &app{cmd: &exec.Cmd{}, healthOK: true}, "running"},
		{"unhealthy", &app{cmd: &exec.Cmd{}}, "unhealthy"},
		{"exited", &app{exited: true}, "stopped"},
	}
	for _, tc := range cases {
		if got := tc.app.statusLocked(); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRecordHealthTransitions(t *testing.T) {
	a := testApp(1)
	failure := errString("connection refused")

	if changed := a.recordHealth(failure, time.Now()); !changed {
		t.Error("first failure should publish the new error")
	}
	a.mu.Lock()
	if !a.healthOK || a.healthFailures != 1 || a.lastError != "connection refused" {
		t.Errorf("after one failure: healthy = %v, failures = %d, error = %q", a.healthOK, a.healthFailures, a.lastError)
	}
	a.mu.Unlock()

	if changed := a.recordHealth(failure, time.Now()); !changed {
		t.Error("second failure should publish the unhealthy state")
	}
	a.mu.Lock()
	if a.healthOK || a.healthFailures != 2 {
		t.Errorf("after two failures: healthy = %v, failures = %d", a.healthOK, a.healthFailures)
	}
	a.mu.Unlock()

	if changed := a.recordHealth(failure, time.Now()); changed {
		t.Error("a repeated failure should not publish again")
	}

	now := time.Now()
	if changed := a.recordHealth(nil, now); !changed {
		t.Error("recovery should publish")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.healthOK || a.healthFailures != 0 || a.lastError != "" || !a.lastHealthy.Equal(now.UTC()) {
		t.Errorf("after recovery: healthy = %v, failures = %d, error = %q, lastHealthy = %s", a.healthOK, a.healthFailures, a.lastError, a.lastHealthy)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCheckHealthMarksAppUnhealthy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "database is down", http.StatusInternalServerError)
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
	a := testApp(port)
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a

	if s.info(a).Status != "running" {
		t.Fatalf("initial status = %q, want running", s.info(a).Status)
	}
	s.checkHealth(a)
	if got := s.info(a).Status; got != "running" {
		t.Errorf("status after one failure = %q, want running until the threshold", got)
	}
	s.checkHealth(a)
	info := s.info(a)
	if info.Status != "unhealthy" {
		t.Errorf("status after two failures = %q, want unhealthy", info.Status)
	}
	if !strings.Contains(info.LastError, "database is down") {
		t.Errorf("last error = %q, want the health response", info.LastError)
	}
}

func TestCheckHealthSkipsStoppedApp(t *testing.T) {
	a := &app{name: "demo", port: 1, cfg: appConfig{Health: "/"}, cmd: nil}
	if changed := a.recordHealth(errString("boom"), time.Now()); changed {
		t.Error("a stopped app should not record health")
	}
}

func TestProxyResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	previous := proxyResponseHeaderTimeout
	proxyResponseHeaderTimeout = 50 * time.Millisecond
	defer func() { proxyResponseHeaderTimeout = previous }()

	s := testSupervisor()
	proxy := s.newProxy(&app{name: "demo", port: port})
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	rec := httptest.NewRecorder()
	started := time.Now()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 after the header timeout", rec.Code)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("proxy waited %s for a hung app", elapsed)
	}
}
