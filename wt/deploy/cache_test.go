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

func TestComponentInputHashIgnoresStaticAndOutputs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main")
	write("static/index.html", "first")
	write("main.wasm", "wasm")
	first, err := componentInputHash(dir, "build")
	if err != nil {
		t.Fatal(err)
	}
	write("static/index.html", "second")
	afterStatic, err := componentInputHash(dir, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterStatic != first {
		t.Error("static change altered the component hash")
	}
	write("main.go", "package main // changed")
	afterGo, err := componentInputHash(dir, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterGo == first {
		t.Error("Go change did not alter the component hash")
	}
}

func TestBundleCanOmitComponent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "index.html"), []byte("spa"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := bundle(dir, manifest{Name: "demo", Host: "demo.localhost"}, false)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	names := []string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	for _, name := range names {
		if name == "main.wasm" || name == "main.cwasm" {
			t.Errorf("static-only bundle contains %s", name)
		}
	}
	if len(names) != 2 {
		t.Errorf("bundle entries = %v, want app.json and static/index.html", names)
	}
}
