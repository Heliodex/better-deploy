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
		if err := os.MkdirAll(filepath.Dir(path), privateDirMode); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main")
	write("static/index.html", "first")
	write("main.wasm", "wasm")

	first, _, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	write("static/index.html", "second")

	afterStatic, _, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	if afterStatic != first {
		t.Error("static change altered the component hash")
	}

	write("main.go", "package main // changed")

	afterGo, _, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	if afterGo == first {
		t.Error("Go change did not alter the component hash")
	}
}

func TestComponentInputHashCoversToolchainSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()

		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), privateDirMode); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main")

	first, count, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Errorf("file count = %d, want the one Go file", count)
	}

	write("asm.s", "TEXT ·x(SB),$0")

	afterAsm, asmCount, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	if afterAsm == first {
		t.Error("assembly change did not alter the component hash")
	}

	if asmCount != 2 {
		t.Errorf("file count = %d, want the Go and assembly files", asmCount)
	}

	write("bridge.c", "int x;")

	afterC, _, err := componentInputHash(dir, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}

	if afterC == afterAsm {
		t.Error("cgo change did not alter the component hash")
	}
}

func TestBundleCanOmitComponent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "static"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "static", "index.html"), []byte("spa"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	data, err := bundle(dir, manifest{Name: testAppName, Host: "demo.localhost"}, false)
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
