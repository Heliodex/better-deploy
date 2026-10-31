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

func TestIPv6EgressConfig(t *testing.T) {
	t.Setenv("WT_IPV6_EGRESS", "off")
	if configFromEnv().ipv6Egress {
		t.Error("WT_IPV6_EGRESS=off should disable IPv6 egress rules")
	}
	t.Setenv("WT_IPV6_EGRESS", "not-a-bool")
	if !configFromEnv().ipv6Egress {
		t.Error("an invalid value should keep the fail-closed default")
	}
	t.Setenv("WT_IP6TABLES", "/sbin/ip6tables")
	if got := configFromEnv().ip6tables; got != "/sbin/ip6tables" {
		t.Errorf("ip6tables = %q", got)
	}
}

func TestUntrustedDefaults(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{}, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryMB != 256 || cfg.DiskMB != 256 {
		t.Errorf("memory/disk = %d/%d, want 256/256", cfg.MemoryMB, cfg.DiskMB)
	}
	if cfg.CPUMillicores != 1000 || cfg.PidsMax != 128 || cfg.IOWeight != 100 {
		t.Errorf("cpu/pids/io = %d/%d/%d", cfg.CPUMillicores, cfg.PidsMax, cfg.IOWeight)
	}
	if cfg.RequestTimeout != "30s" || cfg.requestTimeout != 30*time.Second {
		t.Errorf("timeout = %q (%s), want 30s", cfg.RequestTimeout, cfg.requestTimeout)
	}
	if cfg.Network != "storage" {
		t.Errorf("network = %q, want storage", cfg.Network)
	}
}

func TestUntrustedExplicitTimeoutDisable(t *testing.T) {
	for _, value := range []string{"0", "none"} {
		cfg, err := normalizeAppConfig(appConfig{RequestTimeout: value}, "demo", "localhost", tenantUntrusted)
		if err != nil {
			t.Errorf("%q: %v", value, err)
			continue
		}
		if cfg.requestTimeout != 0 {
			t.Errorf("%q produced timeout %s, want disabled", value, cfg.requestTimeout)
		}
	}
}

func TestUntrustedRejectsInheritNetwork(t *testing.T) {
	if _, err := normalizeAppConfig(appConfig{Network: "inherit"}, "demo", "localhost", tenantUntrusted); err == nil {
		t.Error("network inherit should be rejected in untrusted mode")
	}
	if _, err := normalizeAppConfig(appConfig{Network: "none"}, "demo", "localhost", tenantUntrusted); err != nil {
		t.Errorf("network none should be accepted: %v", err)
	}
	if _, err := normalizeAppConfig(appConfig{Network: "storage"}, "demo", "localhost", tenantUntrusted); err != nil {
		t.Errorf("network storage should be accepted: %v", err)
	}
}

func TestTrustedDefaultsStayUnchanged(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{}, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryMB != 0 || cfg.DiskMB != 0 {
		t.Errorf("trusted memory/disk = %d/%d, want zero", cfg.MemoryMB, cfg.DiskMB)
	}
	if cfg.RequestTimeout != "" || cfg.requestTimeout != 0 {
		t.Errorf("trusted timeout = %q, want unset", cfg.RequestTimeout)
	}
	if cfg.Network != "inherit" {
		t.Errorf("trusted network = %q, want inherit", cfg.Network)
	}
	if cfg.CPUMillicores != 1000 || cfg.PidsMax != 128 || cfg.IOWeight != 100 {
		t.Errorf("new limit defaults = %d/%d/%d", cfg.CPUMillicores, cfg.PidsMax, cfg.IOWeight)
	}
}

func TestConfigResponseIncludesResourceLimits(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{CPUMillicores: 500, PidsMax: 64, IOWeight: 50}, "demo", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	response := configResponse(cfg)
	if response.CPUMillicores != 500 || response.PidsMax != 64 || response.IOWeight != 50 {
		t.Errorf("response = %+v", response)
	}
}

func TestConfigPatchResourceLimitsInUntrustedMode(t *testing.T) {
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	a := &app{name: "demo", dir: t.TempDir(), cfg: appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{}}}
	s.apps["demo"] = a
	s.hosts["demo.localhost"] = a

	patch := `{"cpu_millicores":250,"pids_max":32,"io_weight":25}`
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(patch))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var response appConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CPUMillicores != 250 || response.PidsMax != 32 || response.IOWeight != 25 || !response.RestartRequired {
		t.Errorf("response = %+v", response)
	}
	a.mu.Lock()
	live := a.cfg
	a.mu.Unlock()
	if live.CPUMillicores != 250 || live.PidsMax != 32 || live.IOWeight != 25 {
		t.Errorf("live cfg = %+v", live)
	}
	info, err := os.Stat(filepath.Join(a.dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("app.json mode = %o, want 600 in untrusted mode", got)
	}
}
