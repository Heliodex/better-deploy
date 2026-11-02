package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidHostLabels(t *testing.T) {
	for _, host := range []string{
		"example.com",
		"demo.localhost",
		"a-b.example.com",
		"a1.example.com",
		strings.Repeat("a", 63) + ".example.com",
	} {
		if !validHost(host) {
			t.Errorf("validHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{
		"",
		"localhost",
		"127.0.0.1",
		"::1",
		"-a.example.com",
		"a-.example.com",
		"a..b",
		"a_b.example.com",
		"A.example.com",
		"a/b",
		strings.Repeat("a", 64) + ".example.com",
	} {
		if validHost(host) {
			t.Errorf("validHost(%q) = true, want false", host)
		}
	}
}

func TestValidHostPattern(t *testing.T) {
	for _, pattern := range []string{"*.apps.example.com", "*.example.com", "demo.example.com"} {
		if !validHostPattern(pattern) {
			t.Errorf("validHostPattern(%q) = false, want true", pattern)
		}
	}
	for _, pattern := range []string{
		"*",
		"*.com",
		"*.localhost",
		"*example.com",
		"foo.*.example.com",
		"*.127.0.0.1",
		"*.bad_host.example.com",
	} {
		if validHostPattern(pattern) {
			t.Errorf("validHostPattern(%q) = true, want false", pattern)
		}
	}
}

func TestNormalizeAppHostAliases(t *testing.T) {
	cfg, err := normalizeAppConfig(appConfig{
		Host:  "Demo.LocalHost",
		Hosts: []string{"Old.Example.com", "*.Apps.Example.com", "demo.localhost", "old.example.com"},
	}, "demo", "localhost", tenantTrusted)

	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "demo.localhost" {
		t.Errorf("host = %q, want demo.localhost", cfg.Host)
	}
	if want := []string{"*.apps.example.com", "old.example.com"}; !slices.Equal(cfg.Hosts, want) {
		t.Errorf("hosts = %v, want %v", cfg.Hosts, want)
	}

	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if _, err := normalizeAppConfig(appConfig{Host: host}, "demo", "localhost", tenantTrusted); err == nil {
			t.Errorf("normalizeAppConfig accepted reserved host %q", host)
		}
	}
	if _, err := normalizeAppConfig(appConfig{Host: "apps.example.com"}, "demo", "apps.example.com", tenantTrusted); err == nil {
		t.Error("normalizeAppConfig accepted the supervisor domain root")
	}
	if _, err := normalizeAppConfig(appConfig{Host: "demo.example.com", Hosts: []string{"*.localhost"}}, "demo", "example.com", tenantTrusted); err == nil {
		t.Error("normalizeAppConfig accepted a wildcard without a dotted suffix")
	}
}

func TestHostPatternOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"*.apps.example.com", "*.apps.example.com", true},
		{"*.apps.example.com", "one.apps.example.com", true},
		{"one.apps.example.com", "*.apps.example.com", true},
		{"*.apps.example.com", "two.one.apps.example.com", false},
		{"*.apps.example.com", "*.other.example.com", false},
		{"one.example.com", "two.example.com", false},
		{"one.example.com", "one.example.com", true},
	}
	for _, tc := range cases {
		if got := hostPatternsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("hostPatternsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestHostConflictRejectsReservedAndWildcardOverlap(t *testing.T) {
	s := testSupervisor()
	s.cfg.domain = "apps.example.com"
	s.cfg.listen = "127.0.0.1:8080"
	wildcard := &app{name: "wild", cfg: appConfig{Host: "*.apps.example.com"}}
	s.register(wildcard)

	if err := s.hostConflictError(appConfig{Host: "one.apps.example.com"}, ""); err == nil {
		t.Error("exact child host did not conflict with the wildcard app")
	}
	if err := s.hostConflictError(appConfig{Host: "*.apps.example.com"}, ""); err == nil {
		t.Error("duplicate wildcard host was accepted")
	}
	if err := s.hostConflictError(appConfig{Host: "*.apps.example.com"}, "wild"); err != nil {
		t.Errorf("the owning app conflicted with itself: %v", err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "apps.example.com"} {
		if err := s.hostConflictError(appConfig{Host: host}, ""); err == nil {
			t.Errorf("reserved host %q was accepted", host)
		}
	}
	if err := s.hostConflictError(appConfig{Host: "other.example.com", Hosts: []string{"*.other.example.com"}}, ""); err != nil {
		t.Errorf("unrelated hosts conflicted: %v", err)
	}
}

func TestAppForHostMatchesWildcard(t *testing.T) {
	s := testSupervisor()
	wildcard := &app{name: "wild", cfg: appConfig{Host: "*.apps.example.com"}}
	exact := &app{name: "exact", cfg: appConfig{Host: "one.apps.example.com"}}
	s.register(wildcard)
	s.register(exact)

	if got := s.appForHost("two.apps.example.com"); got != wildcard {
		t.Errorf("wildcard lookup = %v, want wild app", got)
	}
	if got := s.appForHost("one.apps.example.com"); got != exact {
		t.Errorf("exact lookup = %v, want exact app", got)
	}
	if got := s.appForHost("two.one.apps.example.com"); got != nil {
		t.Errorf("multi-label wildcard lookup = %v, want nil", got)
	}
}

func TestHandlerKeepsAdminOnReservedHost(t *testing.T) {
	s := testSupervisor()
	s.cfg.listen = "127.0.0.1:8080"
	shadow := &app{name: "shadow", cfg: appConfig{Host: "localhost"}}
	s.apps["shadow"] = shadow
	s.hosts["localhost"] = shadow
	handler := s.handler()

	req := httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("reserved host healthz = %d %q, want the supervisor health response", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	req.Host = "localhost"
	rec = httptest.NewRecorder()
	s.publicHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("public reserved host = %d, want 404 instead of proxying to the shadow app", rec.Code)
	}
}

func TestAppConfigHostAliases(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{
		Name:  "demo",
		Host:  "demo.example.com",
		Hosts: []string{"*.apps.example.com", "old.example.com"},
		Env:   map[string]string{},
	}, "demo", "example.com", tenantTrusted)

	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.register(a)
	if s.hosts["*.apps.example.com"] != a || s.hosts["old.example.com"] != a {
		t.Fatalf("alias routes missing after register: %v", s.hosts)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"hosts":["new.example.com"]}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH config = %d %s", rec.Code, rec.Body.String())
	}
	var response appConfigResponse
	if err := json.UnmarshalRead(rec.Body, &response); err != nil {
		t.Fatal(err)
	}
	if want := []string{"new.example.com"}; !slices.Equal(response.Hosts, want) {
		t.Errorf("response hosts = %v, want %v", response.Hosts, want)
	}
	if s.hosts["new.example.com"] != a || s.hosts["*.apps.example.com"] != nil || s.hosts["old.example.com"] != nil {
		t.Errorf("host routes after PATCH = %v", s.hosts)
	}
	saved, err := readAppConfig(dir, "demo", "example.com", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.Hosts, []string{"new.example.com"}) {
		t.Errorf("saved hosts = %v", saved.Hosts)
	}
}

func TestInstallStaticUpdatesHostAliases(t *testing.T) {
	s := testSupervisor()
	appDir := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "app.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{
		name: "demo",
		dir:  appDir,
		cfg:  appConfig{Name: "demo", Host: "demo.example.com", Hosts: []string{"old.example.com"}},
	}
	s.register(a)

	next := a.cfg
	next.Hosts = []string{"new.example.com"}
	if err := s.installStatic(a, next, source); err != nil {
		t.Fatal(err)
	}
	if s.hosts["new.example.com"] != a {
		t.Errorf("new alias was not routed: %v", s.hosts)
	}
	if s.hosts["old.example.com"] != nil {
		t.Errorf("old alias was left behind: %v", s.hosts)
	}
	if s.hosts["demo.example.com"] != a {
		t.Errorf("primary host was lost: %v", s.hosts)
	}
}

func TestAppConfigRejectsWildcardAliasConflict(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.example.com", Env: map[string]string{}}, "demo", "example.com", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantTrusted); err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	demo := &app{name: "demo", dir: dir, cfg: cfg}
	s.register(demo)
	other := &app{name: "other", cfg: appConfig{Host: "*.apps.example.com"}}
	s.register(other)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"hosts":["one.apps.example.com"]}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "app.json")); err != nil {
		t.Errorf("app.json missing after rejected patch: %v", err)
	}
}
