package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
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
