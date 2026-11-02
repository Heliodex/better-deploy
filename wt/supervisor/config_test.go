package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppConfigGetAndPatch(t *testing.T) {
	dir := t.TempDir()
	original := `{"name":"demo","host":"demo.localhost","health":"/","env":{"A":"1","SECRET":"x"},"build":"make app","memory_mb":128}`
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := readAppConfig(dir, "demo", "localhost", tenantTrusted)
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

	patch := `{"host":"new.localhost","health":"/ready","env":{"A":"2","SECRET":null,"NEW":"3"},"memory_mb":256,"disk_mb":512,"schedule":[]}`
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
	if got.Host != "new.localhost" || got.Health != "/ready" || got.MemoryMB != 256 || got.DiskMB != 512 || !got.RestartRequired {
		t.Errorf("PATCH config = %+v", got)
	}

	a.mu.Lock()
	live := a.cfg
	a.mu.Unlock()
	if live.Host != "new.localhost" || live.Health != "/ready" || live.MemoryMB != 256 || live.DiskMB != 512 {
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

	saved, err := readAppConfig(dir, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host != "new.localhost" || saved.Health != "/ready" || saved.MemoryMB != 256 || saved.DiskMB != 512 || saved.Build != "make app" {
		t.Errorf("saved config = %+v", saved)
	}
}

func TestAppConfigPatchesHealthSettings(t *testing.T) {
	dir := t.TempDir()
	cfg := appConfig{Name: "demo", Host: "demo.localhost", Health: "/healthz", Env: map[string]string{}}
	cfg, err := normalizeAppConfig(cfg, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.apps["demo"] = a
	s.hosts[cfg.Host] = a

	patch := `{"startup_timeout":"30s","health_interval":"5s","health_failures":3,"restart_on_unhealthy":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(patch))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH config status = %d: %s", rec.Code, rec.Body.String())
	}
	var got appConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StartupTimeout != "30s" || got.HealthInterval != "5s" || got.HealthFailures != 3 || !got.RestartOnUnhealthy {
		t.Errorf("PATCH health config = %+v", got)
	}
	a.mu.Lock()
	live := a.cfg
	a.mu.Unlock()
	if live.startupTimeout != 30*time.Second || live.healthInterval != 5*time.Second {
		t.Errorf("live health durations = %+v", live)
	}
}

func TestAppConfigRejectsTakenHost(t *testing.T) {
	dir := t.TempDir()
	cfg := appConfig{Name: "demo", Host: "demo.localhost", Health: "/", Env: map[string]string{}}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
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

func TestAppConfigPatchesCallersAndEgress(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.localhost", Health: "/", Env: map[string]string{}}, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.apps["demo"] = a
	s.hosts[cfg.Host] = a

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"allow_callers":["Frontend","frontend"]}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("callers patch = %d: %s", rec.Code, rec.Body.String())
	}
	var got appConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.AllowCallers) != 1 || got.AllowCallers[0] != "frontend" || got.RestartRequired {
		t.Errorf("callers patch response = %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"egress":[{"cidr":"203.0.113.9/24","port":443}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("egress patch = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Egress) != 1 || got.Egress[0].CIDR != "203.0.113.0/24" || got.Egress[0].Port != 443 || !got.RestartRequired {
		t.Errorf("egress patch response = %+v", got)
	}

	for _, body := range []string{
		`{"allow_callers":["bad/name"]}`,
		`{"egress":[{"cidr":"not-a-cidr","port":443}]}`,
		`{"egress":[{"cidr":"2001:db8::/32","port":443}]}`,
		`{"egress":[{"cidr":"203.0.113.0/24","port":0}]}`,
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		s.handleAppConfig(rec, req, "demo")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid patch %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestAppConfigConditionalRequests(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.localhost", Health: "/", Env: map[string]string{}}, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
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
		t.Fatalf("GET config = %d: %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET config has no ETag")
	}
	req = httptest.NewRequest(http.MethodGet, "/apps/demo/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"memory_mb":256}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-Match", `"wrong"`)
	rec = httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("stale PATCH = %d, want 412: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"memory_mb":256}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-Match", etag)
	rec = httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("conditional PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" || rec.Header().Get("ETag") == etag {
		t.Errorf("PATCH ETag = %q, old %q", rec.Header().Get("ETag"), etag)
	}
}
