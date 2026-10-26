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

	data, err := bundle(dir)
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
	if entries["app.json"] != `{"name":"hello"}` || entries["main.wasm"] != "wasm" || entries["main.cwasm"] != "cwasm" {
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
