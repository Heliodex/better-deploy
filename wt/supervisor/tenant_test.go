package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicAndAdminHandlersSeparateRoutes(t *testing.T) {
	s := testSupervisor()
	public := s.publicHandler()
	admin := s.adminHandler()

	cases := []struct {
		name    string
		handler http.Handler
		method  string
		target  string
		token   bool
		status  int
	}{
		{"public apps hidden", public, http.MethodGet, "/apps", true, http.StatusNotFound},
		{"public admin hidden", public, http.MethodGet, "/admin/", true, http.StatusNotFound},
		{"public login hidden", public, http.MethodPost, "/login", false, http.StatusNotFound},
		{"public health", public, http.MethodGet, "/healthz", false, http.StatusOK},
		{"admin apps", admin, http.MethodGet, "/apps", true, http.StatusOK},
		{"admin apps unauthenticated", admin, http.MethodGet, "/apps", false, http.StatusUnauthorized},
		{"admin health", admin, http.MethodGet, "/healthz", false, http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{}`))
		if tc.token {
			req.Header.Set("Authorization", "Bearer secret")
		}

		rec := httptest.NewRecorder()
		tc.handler.ServeHTTP(rec, req)

		if rec.Code != tc.status {
			t.Errorf("%s: %s %s = %d, want %d", tc.name, tc.method, tc.target, rec.Code, tc.status)
		}
	}
}

func TestPublicHandlerProxiesAppHosts(t *testing.T) {
	s := testSupervisor()
	backendApp(t, s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("app"))
	}), appConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/anything", nil)
	req.Host = "demo.localhost"
	rec := httptest.NewRecorder()
	s.publicHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "app" {
		t.Errorf("public app host = %d %q, want 200 \"app\"", rec.Code, rec.Body.String())
	}
}

func TestListenAdminUnixSocketIsPrivateAndCleanedUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.sock")

	network, address, err := parseAdminListen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}

	listener, cleanup, err := listenAdmin(network, address)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 600", info.Mode().Perm())
	}

	_ = listener.Close()
	// A closed Unix listener leaves its socket path behind; a second bind must remove the stale socket.
	listener, cleanup2, err := listenAdmin(network, address)
	if err != nil {
		t.Fatalf("rebind over a stale socket: %v", err)
	}

	_ = listener.Close()

	cleanup2()
	cleanup()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still exists after cleanup: %v", err)
	}
}

func TestUntrustedEmptyTokenCannotReachAdmin(t *testing.T) {
	s := testSupervisor()
	s.cfg.token = ""
	s.cfg.tenantMode = tenantUntrusted

	for _, target := range []string{"/apps", "/events"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.adminHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no token = %d, want 401", target, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	s.adminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/ with no token = %d, want the public panel shell", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	s.adminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login with no token = %d, want 401", rec.Code)
	}
}

func TestAdminStaticIsPublicWhileAPIRemainsProtected(t *testing.T) {
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	handler := s.adminHandler()

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/ without a token = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /apps without a token = %d, want 401", rec.Code)
	}
}

func TestValidateTenantConfig(t *testing.T) {
	if err := validateTenantConfig(config{tenantMode: tenantTrusted}); err != nil {
		t.Errorf("trusted without a token should be allowed: %v", err)
	}

	if err := validateTenantConfig(config{tenantMode: tenantUntrusted}); err == nil {
		t.Error("untrusted without a token should be rejected")
	}

	if err := validateTenantConfig(config{tenantMode: tenantUntrusted, token: "secret"}); err != nil {
		t.Errorf("untrusted with a token should be allowed: %v", err)
	}

	if err := validateTenantConfig(config{tenantMode: "open"}); err == nil {
		t.Error("an unknown tenant mode should be rejected")
	}
}

func TestParseAdminListen(t *testing.T) {
	cases := map[string]struct{ network, address string }{
		"127.0.0.1:8081":     {"tcp", "127.0.0.1:8081"},
		"tcp:127.0.0.1:8081": {"tcp", "127.0.0.1:8081"},
		"unix:/tmp/wt.sock":  {"unix", "/tmp/wt.sock"},
	}
	for input, want := range cases {
		network, address, err := parseAdminListen(input)
		if err != nil {
			t.Errorf("%q: %v", input, err)
			continue
		}

		if network != want.network || address != want.address {
			t.Errorf("%q = %s %s, want %s %s", input, network, address, want.network, want.address)
		}
	}

	for _, bad := range []string{"", "unix:", "tcp:"} {
		if _, _, err := parseAdminListen(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
