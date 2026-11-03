package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunRouteCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps/demo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"demo","host":"127.0.0.1","status":"running","ready":true}`))
		case "/":
			w.Header().Set("X-Request-ID", "route-test")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if err := runRoute([]string{"check", "demo", testHostFlag, server.URL}); err != nil {
		t.Fatal(err)
	}
}

func TestRunRouteCheckMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps/demo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"demo","host":"127.0.0.1","status":"running","ready":true}`))
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	err := runRoute([]string{"check", "demo", testHostFlag, server.URL})
	if err == nil || !strings.Contains(err.Error(), "is not routed") {
		t.Errorf("error = %v", err)
	}
}

func TestCheckDeployRouteSkipsLocal(t *testing.T) {
	cfg := &deployConfig{host: "http://127.0.0.1:8080"}
	if err := checkDeployRoute(cfg, manifest{Name: "demo", Host: "demo.localhost", Health: "/healthz"}); err != nil {
		t.Errorf("local route check = %v", err)
	}

	cfg = &deployConfig{host: "https://wt.example.com", skipRouteCheck: true}
	if err := checkDeployRoute(cfg, manifest{Name: "demo", Host: "demo.example.com"}); err != nil {
		t.Errorf("skipped route check = %v", err)
	}
}

func TestLocalHost(t *testing.T) {
	for value, want := range map[string]bool{
		"http://127.0.0.1:8080":  true,
		"passport.localhost":     true,
		"https://wt.example.com": false,
		"example.com":            false,
	} {
		if got := localHost(value); got != want {
			t.Errorf("localHost(%q) = %v, want %v", value, got, want)
		}
	}
}
