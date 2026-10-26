package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readBundleEntries(t *testing.T, bundle []byte) map[string]string {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(bundle))
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
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(data)
	}
	return entries
}

func TestBundleApp(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "spin_manifest_version = 2\n")
	writeTestFile(t, dir, "main.wasm", "wasm-bytes")
	writeTestFile(t, dir, "app.json", `{"host":"example.com"}`)

	bundle, err := bundleApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries := readBundleEntries(t, bundle)
	if len(entries) != 3 {
		t.Fatalf("entries = %v, want 3 files", entries)
	}
	if entries["spin.toml"] != "spin_manifest_version = 2\n" {
		t.Errorf("spin.toml = %q", entries["spin.toml"])
	}
	if entries["main.wasm"] != "wasm-bytes" {
		t.Errorf("main.wasm = %q", entries["main.wasm"])
	}
	if entries["app.json"] != `{"host":"example.com"}` {
		t.Errorf("app.json = %q", entries["app.json"])
	}
}

func TestBundleAppWithoutAppJSON(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "spin_manifest_version = 2\n")
	writeTestFile(t, dir, "main.wasm", "wasm-bytes")

	bundle, err := bundleApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries := readBundleEntries(t, bundle)
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want 2 files", entries)
	}
}

func TestBundleAppMissingManifest(t *testing.T) {
	if _, err := bundleApp(t.TempDir()); err == nil {
		t.Fatal("expected error for missing spin.toml")
	}
}

func TestDefaultAppName(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "[application]\nname = \"gotime\"\nversion = \"0.1.0\"\n")

	name, err := defaultAppName(dir)
	if err != nil {
		t.Fatal(err)
	}
	if name != "gotime" {
		t.Errorf("name = %q, want %q", name, "gotime")
	}
}

func TestDefaultAppNameMissing(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "spin_manifest_version = 2\n")

	if _, err := defaultAppName(dir); err == nil {
		t.Fatal("expected error for missing application name")
	}
}

func TestParseDotEnvLine(t *testing.T) {
	for _, tc := range []struct {
		line       string
		key, value string
		ok         bool
	}{
		{"DEPLOY_HOST=deploy.example.com", "DEPLOY_HOST", "deploy.example.com", true},
		{"  APP = gotime  ", "APP", "gotime", true},
		{`TOKEN="quoted value"`, "TOKEN", "quoted value", true},
		{"TOKEN='single quoted'", "TOKEN", "single quoted", true},
		{"export APP=gotime", "APP", "gotime", true},
		{"PASSWORD=a=b=c", "PASSWORD", "a=b=c", true},
		{"HASH=abc#def", "HASH", "abc#def", true},
		{"EMPTY=", "EMPTY", "", true},
		{"", "", "", false},
		{"# a comment", "", "", false},
		{"no-equals-here", "", "", false},
		{"=no-key", "", "", false},
		{"BAD KEY=value", "", "", false},
	} {
		key, value, ok := parseDotEnvLine(tc.line)
		if key != tc.key || value != tc.value || ok != tc.ok {
			t.Errorf("%q = %q, %q, %v; want %q, %q, %v", tc.line, key, value, ok, tc.key, tc.value, tc.ok)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".env", "# comment\n\nAPP=first\nAPP=second\nTOKEN=secret\n")

	vars := loadDotEnv(dir)
	if vars["APP"] != "first" {
		t.Errorf("APP = %q, want first value to win", vars["APP"])
	}
	if vars["TOKEN"] != "secret" {
		t.Errorf("TOKEN = %q", vars["TOKEN"])
	}
}

func TestLoadDotEnvMissing(t *testing.T) {
	if vars := loadDotEnv(t.TempDir()); len(vars) != 0 {
		t.Errorf("vars = %v, want empty", vars)
	}
}

func TestNewerVersion(t *testing.T) {
	started := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	before := &appState{PID: 10, StartedAt: started}

	if !newerVersion(nil, before) {
		t.Error("a new app should count as newer when there was no previous version")
	}
	if newerVersion(before, &appState{PID: 10, StartedAt: started}) {
		t.Error("the same process should not count as a new version")
	}
	if !newerVersion(before, &appState{PID: 11, StartedAt: started}) {
		t.Error("a new pid should count as a new version")
	}
	if !newerVersion(before, &appState{PID: 10, StartedAt: started.Add(time.Second)}) {
		t.Error("a new start time should count as a new version")
	}
}

func TestFetchAppStateMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()

	state, err := fetchAppState(&deployConfig{host: server.URL, app: "gotime"})
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil for a missing app", state)
	}
}

func TestResolveDeployConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "[application]\nname = \"toml-app\"\n")
	writeTestFile(t, dir, ".env", "APP=file-app\nDEPLOY_HOST=file.example.com\nSUPERVISOR_TOKEN=file-token\n")

	t.Setenv("APP", "env-app")
	t.Setenv("DEPLOY_HOST", "env.example.com")
	t.Setenv("SUPERVISOR_TOKEN", "env-token")

	cfg, err := resolveDeployConfig([]string{"--dir", dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.app != "env-app" || cfg.host != "env.example.com" || cfg.token != "env-token" {
		t.Errorf("cfg = %+v, want process env to beat .env", cfg)
	}

	cfg, err = resolveDeployConfig([]string{"--dir", dir, "--app", "flag-app", "--deploy-host", "flag.example.com", "--token", "flag-token"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.app != "flag-app" || cfg.host != "flag.example.com" || cfg.token != "flag-token" {
		t.Errorf("cfg = %+v, want flags to beat env and .env", cfg)
	}
}

func TestResolveDeployConfigDotEnvFallback(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "spin.toml", "[application]\nname = \"toml-app\"\n")
	writeTestFile(t, dir, ".env", "DEPLOY_HOST=file.example.com\nSUPERVISOR_TOKEN=file-token\n")

	t.Setenv("APP", "")
	t.Setenv("DEPLOY_HOST", "")
	t.Setenv("SUPERVISOR_TOKEN", "")

	cfg, err := resolveDeployConfig([]string{"--dir", dir, "--app", "flag-app"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.app != "flag-app" {
		t.Errorf("app = %q, want flag-app", cfg.app)
	}
	if cfg.host != "file.example.com" || cfg.token != "file-token" {
		t.Errorf("cfg = %+v, want .env fallback", cfg)
	}
}
