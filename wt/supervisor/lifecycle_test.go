package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func spawnTestApp(t *testing.T, s *supervisor, name string) *app {
	t.Helper()
	dir := filepath.Join(s.cfg.appsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.spawn(name, dir, appConfig{
		Name:   name,
		Host:   name + ".localhost",
		Health: "/",
	}, port)
	if err != nil {
		t.Fatal(err)
	}
	s.register(a)
	t.Cleanup(func() { s.stop(a) })
	return a
}

func newLifecycleSupervisor(t *testing.T) *supervisor {
	t.Helper()
	s := testSupervisor()
	s.cfg.wasmtime = os.Args[0]
	s.cfg.appsDir = t.TempDir()
	s.cfg.stateDir = t.TempDir()
	return s
}

func TestAppStopAndStart(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "demo")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/stop", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleStop(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d: %s", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	retired, running := a.retired, a.cmd != nil && !a.exited
	a.mu.Unlock()
	if !retired || running {
		t.Errorf("after stop retired = %v, running = %v", retired, running)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/apps/demo/start", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleStart(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	retired, running = a.retired, a.cmd != nil && !a.exited
	a.mu.Unlock()
	if retired || !running {
		t.Errorf("after start retired = %v, running = %v", retired, running)
	}
}

func TestDeleteAppKeepsStateByDefault(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "demo")
	appDir := a.dir
	stateFile := filepath.Join(s.cfg.stateDir, "demo", "data.txt")
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apps/demo", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if s.lookup("demo") != nil || s.hosts["demo.localhost"] != nil {
		t.Error("deleted app is still registered")
	}
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Errorf("app directory still exists: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("state file should be kept: %v", err)
	}
}

func TestDeleteAppPurgesState(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "demo")
	appDir := a.dir
	for _, dir := range []string{
		filepath.Join(s.cfg.stateDir, "demo"),
		filepath.Join(s.cfg.stateDir, ".logs", "demo"),
		filepath.Join(s.cfg.stateDir, ".storage", "demo"),
		filepath.Join(s.cfg.stateDir, ".backups", "demo"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resource := filepath.Join(s.cfg.stateDir, ".resources", "demo.json")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apps/demo?purge=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleDelete(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Errorf("app directory still exists: %v", err)
	}
	for _, path := range []string{
		filepath.Join(s.cfg.stateDir, "demo"),
		filepath.Join(s.cfg.stateDir, ".logs", "demo"),
		filepath.Join(s.cfg.stateDir, ".storage", "demo"),
		filepath.Join(s.cfg.stateDir, ".backups", "demo"),
		resource,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
}
