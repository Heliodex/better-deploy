package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppConfigGetAndPatch(t *testing.T) {
	dir := t.TempDir()
	original := `{"name":"demo","host":"demo.localhost","health":"/","env":{"A":"1","SECRET":"x"},"build":"make app","memory_mb":128}`
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := readAppConfig(dir, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.apps["demo"] = a
	s.hosts[cfg.Host] = a

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"x"`) {
		t.Errorf("GET config leaked an env value: %s", rec.Body.String())
	}
	var got appConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != "demo.localhost" || got.MemoryMB != 128 || len(got.EnvKeys) != 2 {
		t.Errorf("GET config = %+v", got)
	}

	patch := `{"host":"new.localhost","health":"/ready","env":{"A":"2","SECRET":null,"NEW":"3"},"memory_mb":256,"schedule":[]}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(patch))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH config status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != "new.localhost" || got.Health != "/ready" || got.MemoryMB != 256 || !got.RestartRequired {
		t.Errorf("PATCH config = %+v", got)
	}

	a.mu.Lock()
	live := a.cfg
	a.mu.Unlock()
	if live.Host != "new.localhost" || live.Health != "/ready" || live.MemoryMB != 256 {
		t.Errorf("live config = %+v", live)
	}
	if live.Env["A"] != "2" || live.Env["NEW"] != "3" {
		t.Errorf("live env = %v", live.Env)
	}
	if _, ok := live.Env["SECRET"]; ok {
		t.Error("null env value did not remove the key")
	}
	if s.hosts["new.localhost"] != a || s.hosts["demo.localhost"] != nil {
		t.Errorf("hosts map was not updated: %v", s.hosts)
	}

	saved, err := readAppConfig(dir, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host != "new.localhost" || saved.Health != "/ready" || saved.MemoryMB != 256 || saved.Build != "make app" {
		t.Errorf("saved config = %+v", saved)
	}
}

func TestAppConfigRejectsTakenHost(t *testing.T) {
	dir := t.TempDir()
	cfg := appConfig{Name: "demo", Host: "demo.localhost", Health: "/", Env: map[string]string{}}
	if err := persistAppConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.apps["demo"] = a
	s.hosts[cfg.Host] = a
	other := &app{name: "other", cfg: appConfig{Host: "taken.localhost"}}
	s.apps["other"] = other
	s.hosts["taken.localhost"] = other

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"host":"taken.localhost"}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
