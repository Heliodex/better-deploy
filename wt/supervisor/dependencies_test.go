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

func TestReadyzDistinctFromLiveness(t *testing.T) {
	s := testSupervisor()
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before ready = %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", rec.Code)
	}

	s.ready.Store(true)
	rec = httptest.NewRecorder()
	s.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("readyz = %d %q", rec.Code, rec.Body.String())
	}
	s.stopping.Store(true)
	rec = httptest.NewRecorder()
	s.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz while stopping = %d, want 503", rec.Code)
	}
}

func TestDependenciesGateRouting(t *testing.T) {
	s := testSupervisor()
	web := &app{name: "web", cfg: appConfig{DependsOn: []string{"api"}}}
	s.apps["web"] = web
	s.hosts["web.localhost"] = web

	ready, waiting := s.dependenciesReady(web)
	if ready || len(waiting) != 1 || waiting[0] != "api" {
		t.Fatalf("dependenciesReady = %v %v", ready, waiting)
	}
	rec := httptest.NewRecorder()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://web.localhost/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "api") {
		t.Errorf("gated response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	api := &app{name: "api", cfg: appConfig{}, cmd: &exec.Cmd{}, ready: true}
	s.apps["api"] = api
	if ready, waiting := s.dependenciesReady(web); !ready {
		t.Errorf("ready with dependency = %v %v", ready, waiting)
	}

	api.mu.Lock()
	api.cfg.Maintenance = true
	api.mu.Unlock()
	if ready, _ := s.dependenciesReady(web); ready {
		t.Error("a dependency in maintenance still counted as ready")
	}
}

func TestDependencyCycles(t *testing.T) {
	s := testSupervisor()
	a := &app{name: "a", cfg: appConfig{DependsOn: []string{"b"}}}
	b := &app{name: "b", cfg: appConfig{DependsOn: []string{"a"}}}
	s.apps["a"] = a
	s.apps["b"] = b
	if err := s.dependencyCycleError("a", []string{"b"}); err == nil {
		t.Error("cycle a -> b -> a was accepted")
	}
	if err := s.dependencyCycleError("a", []string{"missing"}); err != nil {
		t.Errorf("forward reference rejected: %v", err)
	}
	b.mu.Lock()
	b.cfg.DependsOn = nil
	b.mu.Unlock()
	if err := s.dependencyCycleError("a", []string{"b"}); err != nil {
		t.Errorf("acyclic graph rejected: %v", err)
	}
	if _, err := normalizeDependsOn([]string{"a"}, "a"); err == nil {
		t.Error("self dependency was accepted")
	}
}

func TestMaintenanceEndpointAndPage(t *testing.T) {
	s := testSupervisor()
	s.cfg.appsDir = t.TempDir()
	s.cfg.stateDir = t.TempDir()
	dir := filepath.Join(s.cfg.appsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{name: "demo", dir: dir, cfg: appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{}}}
	s.apps["demo"] = a
	s.hosts["demo.localhost"] = a

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/maintenance", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppMaintenance(rec, req, "demo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("maintenance POST = %d %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"maintenance":true`) {
		t.Errorf("app.json = %s", data)
	}

	rec = httptest.NewRecorder()
	s.handleProxy(rec, httptest.NewRequest(http.MethodGet, "http://demo.localhost/some/page", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("maintenance page = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "300" {
		t.Errorf("Retry-After = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "demo is under maintenance") {
		t.Errorf("page = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/apps/demo/maintenance", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppMaintenance(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("maintenance off = %d %s", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	maintenance := a.cfg.Maintenance
	a.mu.Unlock()
	if maintenance {
		t.Error("maintenance stayed enabled")
	}
}
