package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundle(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"app.json":   `{"name":"hello"}`,
		"main.wasm":  "wasm",
		"main.cwasm": "cwasm",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	data, err := bundle(dir, manifest{Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	entries := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(body)
	}
	if !strings.Contains(entries["app.json"], `"name": "hello"`) || entries["main.wasm"] != "wasm" || entries["main.cwasm"] != "cwasm" {
		t.Errorf("entries = %v", entries)
	}
}

func TestBaseURL(t *testing.T) {
	for input, want := range map[string]string{
		"http://127.0.0.1:8080":  "http://127.0.0.1:8080",
		"https://deploy.example": "https://deploy.example",
		"deploy.example":         "https://deploy.example",
		"http://127.0.0.1:8080/": "http://127.0.0.1:8080",
	} {
		if got := baseURL(input); got != want {
			t.Errorf("baseURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveDevConfig(t *testing.T) {
	cfg, err := resolveDevConfig([]string{"--dir", "guestbook", "--port", "9000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dir != "guestbook" || cfg.port != 9000 {
		t.Errorf("config = %+v, want dir guestbook port 9000", cfg)
	}
	if cfg.appHost != "127.0.0.1" {
		t.Errorf("appHost = %q, want 127.0.0.1", cfg.appHost)
	}

	cfg, err = resolveDevConfig([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dir != "." || cfg.port != 8080 {
		t.Errorf("defaults = %+v, want dir . port 8080", cfg)
	}

	t.Setenv("WT_DEV_PORT", "9001")
	cfg, err = resolveDevConfig([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.port != 9001 {
		t.Errorf("port = %d, want 9001 from WT_DEV_PORT", cfg.port)
	}

	t.Setenv("WT_DEV_PORT", "bogus")
	if _, err := resolveDevConfig([]string{}); err == nil {
		t.Error("expected error for invalid WT_DEV_PORT")
	}
}

func TestFindSupervisor(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "wt-supervisor")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &devConfig{supervisor: bin}
	if got, err := findSupervisor(cfg, t.TempDir()); err != nil || got != bin {
		t.Errorf("findSupervisor = %q, %v; want %q, nil", got, err, bin)
	}

	cfg = &devConfig{supervisor: filepath.Join(t.TempDir(), "missing")}
	if _, err := findSupervisor(cfg, t.TempDir()); err == nil {
		t.Error("expected error for missing supervisor binary")
	}
}

func TestDevEnv(t *testing.T) {
	t.Setenv("WT_TOKEN", "prod-token")
	t.Setenv("WT_TLS_CERT", "/tmp/cert")
	env := devEnv("/apps", "/state", "127.0.0.1:8080", "/bin/wasmtime", "triple")
	got := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}
	for key, want := range map[string]string{
		"WT_APPS_DIR":        "/apps",
		"WT_STATE_DIR":       "/state",
		"WT_LISTEN":          "127.0.0.1:8080",
		"WT_WASMTIME":        "/bin/wasmtime",
		"WT_WASMTIME_TARGET": "triple",
		"WT_TOKEN":           "dev",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for _, key := range []string{"WT_TLS_CERT", "WT_TLS_KEY"} {
		if got[key] != "" {
			t.Errorf("%s = %q, want empty", key, got[key])
		}
	}
	if got["WT_TOKEN"] == "prod-token" {
		t.Error("parent WT_TOKEN leaked into dev env")
	}
}
