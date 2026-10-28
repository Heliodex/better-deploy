package main

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExampleLayout(t *testing.T, root string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "example", "go.mod"), []byte("module example.test\n\ngo 1.27.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages", "go.mod"), []byte("module wt.packages\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunNew(t *testing.T) {
	root := t.TempDir()
	writeExampleLayout(t, root)
	dir := filepath.Join(root, "example", "myapp")

	if err := runNew([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"app.json",
		"main.go",
		".gitignore",
		"contract/generate.sh",
		"web/package.json",
		"web/src/lib/api.ts",
		"web/src/lib/contract.ts",
		"web/scripts/contract.ts",
		"web/src/routes/+page.svelte",
		"web/src/routes/+layout.svelte",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "myapp" || manifest.Host != "myapp.localhost" {
		t.Errorf("manifest = %+v", manifest)
	}

	main, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), `"example.test/myapp/contract"`) {
		t.Errorf("main.go does not import the app contract:\n%s", main)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", main, parser.AllErrors); err != nil {
		t.Errorf("main.go does not parse: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "contract", "generate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("generate.sh is not executable: %v", info.Mode())
	}

	if err := runNew([]string{"--dir", dir}); err == nil {
		t.Error("expected error scaffolding into a non-empty directory")
	}
	if err := runNew([]string{"--dir", filepath.Join(root, "example", "Bad_Name")}); err == nil {
		t.Error("expected error for an invalid app name")
	}
	if err := runNew([]string{"--dir", filepath.Join(t.TempDir(), "elsewhere")}); err == nil {
		t.Error("expected error outside wt/example")
	}
}
