package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAppDirFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "wt", "example", "pantry")

	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(filepath.Join(root, "wt"))

	got := resolveAppDir("wt/example/pantry")
	if got != app {
		t.Errorf("resolveAppDir = %q, want %q", got, app)
	}

	if got := resolveAppDir(""); got != "" {
		t.Errorf("resolveAppDir(empty) = %q, want empty", got)
	}
}
