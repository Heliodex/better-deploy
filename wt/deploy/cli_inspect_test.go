package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"name":"demo","host":"old.example.com","memory_mb":128,"server_paths":["/auth"],"server_pages":["/"],"env":{"A":"one"}}`

	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(manifest), privateFileMode); err != nil {
		t.Fatal(err)
	}

	comparison := compareManifest(dir, "demo", map[string]any{
		"host":             "new.example.com",
		"memory_mb":        float64(256),
		"server_paths":     []any{"/auth"},
		"server_pages":     []any{"/login"},
		"env_keys":         []any{"A", "B"},
		"restart_required": false,
	})
	if comparison == nil {
		t.Fatal("comparison is nil")
	}

	joined := strings.Join(comparison.Differences, "\n")
	for _, want := range []string{"host:", "memory_mb:", "server_pages:", "env keys:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("differences missing %q:\n%s", want, joined)
		}
	}
}

func TestRunInspect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/capabilities":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"dev","wasmtime":"wasmtime 50.0.0","wasmtimeTarget":"x86_64-test","compiler":true}`))
		case "/apps/demo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"demo","host":"127.0.0.1","status":"running","ready":true,"currentDigest":"abcdef0123456789"}`))
		case "/apps/demo/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"host":"127.0.0.1","health":"/healthz","memory_mb":128,"server_paths":["/auth"],"env_keys":["A"],"secret_keys":[],"egress":[],"schedule":[]}`))
		case "/apps/demo/deployments":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"one","mode":"full","runtime":"wasmtime 50.0.0","target":"x86_64-test","deployedAt":"2026-11-03T02:00:00Z"}]`))
		case "/":
			w.Header().Set("X-Request-ID", "inspect-test")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"127.0.0.1","memory_mb":128}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := runInspect([]string{"demo", testHostFlag, server.URL, "--dir", dir}); err != nil {
		t.Fatal(err)
	}
}

func TestInspectConfigDecodesUnknownFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"host":"demo.localhost","future_field":{"nested":true}}`))
	}))
	defer server.Close()

	config, err := inspectConfig(clientOptions{host: server.URL, timeout: 0}, "demo")
	if err != nil {
		t.Fatal(err)
	}

	if config["host"] != "demo.localhost" {
		t.Errorf("config = %+v", config)
	}

	if _, err := json.Marshal(config); err != nil {
		t.Errorf("config does not marshal: %v", err)
	}
}
