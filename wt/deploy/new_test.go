package main

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
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

// TestNewMatchesTemplate fails if a template file is missing from the embed
// list in new.go, or if node_modules or bun.lock leak into the embedded set.
func TestNewMatchesTemplate(t *testing.T) {
	embedded := map[string]bool{}
	err := fs.WalkDir(templateFS, "testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "testdata/template/")
		embedded[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	onDisk := map[string]bool{}
	err = filepath.WalkDir("testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Join("testdata", "template"), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == "web" || strings.HasPrefix(rel, "web/") {
			name := strings.TrimPrefix(rel, "web/")
			if name == "node_modules" || strings.HasPrefix(name, "node_modules/") || name == "bun.lock" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// Generated output that bun install and svelte-kit sync leave behind.
			if name == ".svelte-kit" || strings.HasPrefix(name, ".svelte-kit/") || name == "build" || strings.HasPrefix(name, "build/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() {
			return nil
		}
		onDisk[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for rel := range onDisk {
		if !embedded[rel] {
			t.Errorf("template file %s is not embedded; add it to the go:embed list in new.go", rel)
		}
	}
	for rel := range embedded {
		if !onDisk[rel] {
			t.Errorf("embedded file %s is missing on disk", rel)
		}
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
