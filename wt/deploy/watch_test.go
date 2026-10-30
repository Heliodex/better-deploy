package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevSourceStateIgnoresOutputs(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go")
	write("main.wasm")
	write("main.cwasm")
	write("artifact.json")
	write("static/index.html")
	write("node_modules/pkg/index.js")

	state, err := devSourceState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 {
		t.Fatalf("state = %v, want only main.go", state)
	}
	if _, ok := state[filepath.Join(dir, "main.go")]; !ok {
		t.Errorf("state missing main.go: %v", state)
	}

	write("main.go")
	next, err := devSourceState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !devSourcesChanged(state, next) {
		t.Error("editing a source file was not detected")
	}

	write("static/other.html")
	after, err := devSourceState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if devSourcesChanged(next, after) {
		t.Error("static output triggered a rebuild")
	}
}
