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
		ready:    true,
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
		{"healthy", &app{cmd: &exec.Cmd{}, healthOK: true, ready: true}, "running"},
		{"unready", &app{cmd: &exec.Cmd{}, healthOK: true}, "unhealthy"},
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
	if !a.healthOK || !a.ready || a.healthFailures != 1 || a.lastError != "connection refused" {
		t.Errorf("after one failure: healthy = %v, ready = %v, failures = %d, error = %q", a.healthOK, a.ready, a.healthFailures, a.lastError)
	}
	a.mu.Unlock()

	if changed := a.recordHealth(failure, time.Now()); !changed {
		t.Error("second failure should publish the unhealthy state")
	}
	a.mu.Lock()
	if a.healthOK || a.ready || a.healthFailures != 2 {
		t.Errorf("after two failures: healthy = %v, ready = %v, failures = %d", a.healthOK, a.ready, a.healthFailures)
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
	if !a.healthOK || !a.ready || a.healthFailures != 0 || a.lastError != "" || !a.lastHealthy.Equal(now.UTC()) {
		t.Errorf("after recovery: healthy = %v, ready = %v, failures = %d, error = %q, lastHealthy = %s", a.healthOK, a.ready, a.healthFailures, a.lastError, a.lastHealthy)
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

func TestRecordHealthHonorsFailureLimit(t *testing.T) {
	a := testApp(1)
	a.cfg.HealthFailures = 3

	for range 2 {
		a.recordHealth(errString("boom"), time.Now())
	}
	a.mu.Lock()
	if !a.ready {
		t.Error("app became unready before the configured failure count")
	}
	a.mu.Unlock()
	a.recordHealth(errString("boom"), time.Now())
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		t.Error("app stayed ready after the configured failure count")
	}
}

func TestCheckAllHealthHonorsInterval(t *testing.T) {
	s := testSupervisor()
	a := testApp(1)
	a.nextHealth = time.Now().Add(time.Minute)
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a

	s.checkAllHealth()
	a.mu.Lock()
	failures := a.healthFailures
	a.mu.Unlock()
	if failures != 0 {
		t.Errorf("probe ran before the health interval: failures = %d", failures)
	}

	a.mu.Lock()
	a.nextHealth = time.Time{}
	a.mu.Unlock()
	s.checkAllHealth()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.healthFailures != 1 {
		t.Errorf("failures = %d, want a due probe", a.healthFailures)
	}
}

func TestReadinessBlocksTrafficUntilRecovery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	a := &app{name: "demo", port: port, cfg: appConfig{Health: "/"}, cmd: &exec.Cmd{}, healthOK: true, ready: true}
	a.proxy = s.newProxy(a)
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a

	rec := httptest.NewRecorder()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ready app status = %d, want 204", rec.Code)
	}

	a.mu.Lock()
	a.ready = false
	a.healthOK = false
	a.mu.Unlock()
	rec = httptest.NewRecorder()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unready app status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("unready response has no Retry-After")
	}

	s.checkHealth(a)
	rec = httptest.NewRecorder()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("recovered app status = %d, want 204", rec.Code)
	}
}

func TestRestartOnUnhealthyKillsProcess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusInternalServerError)
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

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	s := testSupervisor()
	a := &app{
		name:           "demo",
		port:           port,
		cfg:            appConfig{Health: "/", HealthFailures: 1, RestartOnUnhealthy: true},
		cmd:            cmd,
		done:           done,
		healthOK:       true,
		ready:          true,
		healthFailures: 0,
	}
	s.apps[a.name] = a
	s.hosts["demo.localhost"] = a

	s.checkHealth(a)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unhealthy app was not restarted")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		t.Error("app stayed ready after a failed health check")
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
