package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "app-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)

	for name, body := range entries {
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestExtractTarGz(t *testing.T) {
	archive := writeTarGz(t, map[string]string{
		"spin.toml": "spin_manifest_version = 2\n",
		"main.wasm": "not really wasm",
	})
	dst := t.TempDir()

	if err := extractTarGz(archive, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spin.toml", "main.wasm"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	archive := writeTarGz(t, map[string]string{
		"../evil": "nope",
	})
	dst := t.TempDir()

	if err := extractTarGz(archive, dst); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); err == nil {
		t.Fatal("traversal wrote outside the destination")
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"gotime", "my-app", "a1"} {
		if !validName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "-app", "app-", "App", "app_name", "app.name", "../app"} {
		if validName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestReadAppConfigDefault(t *testing.T) {
	dir := t.TempDir()
	ac, err := readAppConfig(dir, "gotime", "apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ac.Host != "gotime.apps.example.com" {
		t.Errorf("host = %q", ac.Host)
	}
	if ac.Health != "/" {
		t.Errorf("health = %q", ac.Health)
	}
}

func TestSplitListen(t *testing.T) {
	for _, tc := range []struct {
		in, host, port string
	}{
		{"0.0.0.0:8080", "0.0.0.0", "8080"},
		{":8080", "", "8080"},
		{"127.0.0.1:9000", "127.0.0.1", "9000"},
		{"http://:8080", "", "8080"},
	} {
		host, port, err := splitListen(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if host != tc.host || port != tc.port {
			t.Errorf("%q = %q, %q; want %q, %q", tc.in, host, port, tc.host, tc.port)
		}
	}
}

func TestValidateHost(t *testing.T) {
	for _, host := range []string{"gotime.apps.example.com", "localhost", "a-b.example"} {
		if err := validateHost(host); err != nil {
			t.Errorf("%q: %v", host, err)
		}
	}
	for _, host := range []string{"", ".example", "example.", "a..b", "bad_host", "bad host", "http://example"} {
		if err := validateHost(host); err == nil {
			t.Errorf("%q should be invalid", host)
		}
	}
}

func TestWriteCaddyfile(t *testing.T) {
	dir := t.TempDir()
	s := &Supervisor{
		cfg: config{
			caddyConfig: filepath.Join(dir, "Caddyfile"),
			caddyAdmin:  "127.0.0.1:2019",
			listen:      "127.0.0.1:8080",
			apiListen:   "127.0.0.1:9090",
			deployHost:  "deploy.example.com",
		},
		apps: map[string]*App{
			"gotime": {Name: "gotime", Host: "gotime.example.com", Port: 3100},
		},
	}
	if err := s.writeCaddyfile(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.cfg.caddyConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"http://:8080",
		"bind 127.0.0.1",
		"@health path /healthz",
		"@deploy host deploy.example.com",
		"@app0 host gotime.example.com",
		"reverse_proxy 127.0.0.1:3100",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("Caddyfile missing %q:\n%s", want, data)
		}
	}
}
