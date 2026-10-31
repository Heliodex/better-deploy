package main

import (
	"net/http"
	"net/http/httptest"
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

func TestUntrustedEmptyTokenCannotReachAdmin(t *testing.T) {
	s := testSupervisor()
	s.cfg.token = ""
	s.cfg.tenantMode = tenantUntrusted

	for _, target := range []string{"/apps", "/events", "/admin/"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.adminHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no token = %d, want 401", target, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.adminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login with no token = %d, want 401", rec.Code)
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
