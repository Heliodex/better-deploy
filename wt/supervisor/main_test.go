package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
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
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
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

func TestValidName(t *testing.T) {
	for _, name := range []string{"hello", "notes", "my-app", "a1"} {
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

func TestExtractTarGz(t *testing.T) {
	archive := writeTarGz(t, map[string]string{"app.json": "{}", "main.wasm": "wasm"})
	dst := t.TempDir()
	if err := extractTarGz(archive, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.json", "main.wasm"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	archive := writeTarGz(t, map[string]string{"../evil": "nope"})
	if err := extractTarGz(archive, t.TempDir()); err == nil {
		t.Fatal("expected traversal error")
	}
}
