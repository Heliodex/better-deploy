package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseIdleTimeout(t *testing.T) {
	timeout, err := parseIdleTimeout("")
	if err != nil || timeout != defaultIdleTimeout {
		t.Errorf("parseIdleTimeout(%q) = %s, %v; want %s, nil", "", timeout, err, defaultIdleTimeout)
	}

	for _, value := range []string{"0", "none", "off", "OFF"} {
		timeout, err := parseIdleTimeout(value)
		if err != nil || timeout != 0 {
			t.Errorf("parseIdleTimeout(%q) = %s, %v; want 0, nil", value, timeout, err)
		}
	}

	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"10s", 10 * time.Second},
		{"15m", 15 * time.Minute},
		{"24h", 24 * time.Hour},
	} {
		timeout, err := parseIdleTimeout(tc.value)
		if err != nil || timeout != tc.want {
			t.Errorf("parseIdleTimeout(%q) = %s, %v; want %s, nil", tc.value, timeout, err, tc.want)
		}
	}

	for _, value := range []string{"9s", "25h", "soon", "-1m"} {
		if _, err := parseIdleTimeout(value); err == nil {
			t.Errorf("parseIdleTimeout(%q) succeeded; want an error", value)
		}
	}
}

func TestIdleDue(t *testing.T) {
	now := time.Now()

	newApp := func() *app {
		return &app{
			name:    "demo",
			cfg:     appConfig{idleTimeout: 10 * time.Second},
			cmd:     &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
			started: now.Add(-11 * time.Second),
		}
	}

	if !newApp().idleDue(now) {
		t.Error("an app idle past its timeout should be due")
	}

	a := newApp()

	a.lastRequest = now.Add(-time.Second)
	if a.idleDue(now) {
		t.Error("a recent request should keep the app running")
	}

	a = newApp()

	a.inflight = 1
	if a.idleDue(now) {
		t.Error("in-flight work should keep the app running")
	}

	a = newApp()

	a.cfg.Schedule = []scheduleConfig{{Path: "/tick", Every: "1m"}}
	if a.idleDue(now) {
		t.Error("an app with scheduled calls should not scale to zero")
	}

	a = newApp()

	a.retired = true
	if a.idleDue(now) {
		t.Error("a stopped app should not be due again")
	}

	a = newApp()

	a.idleStopped = true
	if a.idleDue(now) {
		t.Error("an already idle-stopped app should not be due")
	}

	a = newApp()

	a.cfg.idleTimeout = 0
	if a.idleDue(now) {
		t.Error("an app without idle_timeout should not be due")
	}
}

func TestMarkIdleStopped(t *testing.T) {
	now := time.Now()
	a := &app{
		name:    "demo",
		cfg:     appConfig{idleTimeout: 10 * time.Second},
		cmd:     &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
		started: now.Add(-11 * time.Second),
	}

	if !a.markIdleStopped() {
		t.Fatal("markIdleStopped should accept an idle app")
	}

	if !a.idleState() {
		t.Error("markIdleStopped did not set the idle flag")
	}

	if a.markIdleStopped() {
		t.Error("markIdleStopped should not accept an already idle app")
	}
}

func TestServeAppWakesIdleApp(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "demo")

	a.mu.Lock()
	a.cfg.idleTimeout = 10 * time.Second
	a.started = time.Now().Add(-time.Minute)
	a.mu.Unlock()

	s.stopIdleApps(time.Now())

	if !a.idleState() {
		t.Fatal("idle app was not stopped")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	s.serveApp(rec, req, a)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 after waking: %s", rec.Code, rec.Body.String())
	}

	if a.idleState() {
		t.Error("app stayed marked idle after a request")
	}
}

func TestLoadAppsStartsAsleepByDefault(t *testing.T) {
	s := newLifecycleSupervisor(t)

	dir := filepath.Join(s.cfg.appsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.loadApps(); err != nil {
		t.Fatal(err)
	}

	a := s.lookup("demo")
	if a == nil {
		t.Fatal("app was not loaded")
	}

	t.Cleanup(func() { s.stop(a) })

	a.mu.Lock()
	running, idle, ready, proxy, timeout := a.cmd != nil, a.idleStopped, a.ready, a.proxy, a.cfg.idleTimeout
	a.mu.Unlock()

	if running || !idle || ready || proxy != nil {
		t.Errorf("loaded app running=%v idle=%v ready=%v proxy=%v; want asleep", running, idle, ready, proxy != nil)
	}

	if timeout != defaultIdleTimeout {
		t.Errorf("idle timeout = %s, want the %s default", timeout, defaultIdleTimeout)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	s.serveApp(rec, req, a)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 after waking: %s", rec.Code, rec.Body.String())
	}

	a.mu.Lock()
	idle, running = a.idleStopped, a.cmd != nil
	a.mu.Unlock()

	if idle || !running {
		t.Errorf("after the first request idle=%v running=%v; want awake", idle, running)
	}
}

func TestLoadAppsStartsWithExplicitOptOut(t *testing.T) {
	s := newLifecycleSupervisor(t)

	dir := filepath.Join(s.cfg.appsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost","idle_timeout":"none"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.loadApps(); err != nil {
		t.Fatal(err)
	}

	a := s.lookup("demo")
	if a == nil {
		t.Fatal("app was not loaded")
	}

	t.Cleanup(func() { s.stop(a) })

	a.mu.Lock()
	running, idle, timeout := a.cmd != nil, a.idleStopped, a.cfg.idleTimeout
	a.mu.Unlock()

	if !running || idle || timeout != 0 {
		t.Errorf("opt-out app running=%v idle=%v timeout=%s; want running and not idle", running, idle, timeout)
	}
}

func TestStopIdleAppsStopsProcess(t *testing.T) {
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

	now := time.Now()
	a := &app{
		name:    "demo",
		cfg:     appConfig{idleTimeout: 10 * time.Second},
		cmd:     cmd,
		done:    done,
		started: now.Add(-time.Minute),
	}

	s := testSupervisor()
	s.apps[a.name] = a

	s.stopIdleApps(now)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle app process was not stopped")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.idleStopped || !a.retired {
		t.Errorf("idle stop left idleStopped=%v retired=%v; want both true", a.idleStopped, a.retired)
	}
}

func TestIdleAppServesStaticWithoutWaking(t *testing.T) {
	dir := t.TempDir()

	static := filepath.Join(dir, "static")
	if err := os.MkdirAll(static, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("<h1>static</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &app{name: "demo", dir: dir, cfg: appConfig{Health: "/healthz"}, idleStopped: true}

	s := testSupervisor()
	s.apps[a.name] = a

	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/", nil)
	req.Header.Set("Accept", "text/html")

	rec := httptest.NewRecorder()
	s.serveApp(rec, req, a)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if rec.Body.String() != "<h1>static</h1>" {
		t.Errorf("body = %q, want the static page", rec.Body.String())
	}

	if !a.idleState() {
		t.Error("serving a static file woke the idle app")
	}
}
