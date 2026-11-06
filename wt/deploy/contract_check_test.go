package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContractTree writes a contract.gen.go and schema files under dir.
func writeContractTree(t *testing.T, dir, goBody string, schemas map[string]string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, "schemas"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "contract.gen.go"), []byte(goBody), privateFileMode); err != nil {
		t.Fatal(err)
	}

	for name, body := range schemas {
		if err := os.WriteFile(filepath.Join(dir, "schemas", name), []byte(body), privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompareContractTrees(t *testing.T) {
	baseSchemas := map[string]string{
		"note.json": `{"title":"Note"}`,
	}

	tests := []struct {
		name       string
		generated  map[string]string
		current    map[string]string
		wantErrors []string
	}{
		{
			name:      "matching",
			generated: baseSchemas,
			current:   baseSchemas,
		},
		{
			name:       "missing schema",
			generated:  baseSchemas,
			current:    map[string]string{},
			wantErrors: []string{"contract/schemas/note.json is missing"},
		},
		{
			name:       "stale schema",
			generated:  map[string]string{},
			current:    baseSchemas,
			wantErrors: []string{"contract/schemas/note.json is stale"},
		},
		{
			name:       "different schema",
			generated:  baseSchemas,
			current:    map[string]string{"note.json": `{"title":"Old"}`},
			wantErrors: []string{"contract/schemas/note.json differs"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			generated := filepath.Join(root, "generated")
			current := filepath.Join(root, "current")

			writeContractTree(t, generated, "package contract\n", test.generated)
			writeContractTree(t, current, "package contract\n", test.current)

			err := compareContractTrees(generated, current)
			if len(test.wantErrors) == 0 {
				if err != nil {
					t.Fatalf("compareContractTrees() = %v", err)
				}

				return
			}

			if err == nil {
				t.Fatal("compareContractTrees() passed, want an error")
			}

			for _, want := range test.wantErrors {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestCompareContractTreesReportsChangedGoFile(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated")
	current := filepath.Join(root, "current")

	writeContractTree(t, generated, "package contract\n\ntype Note struct{}\n", nil)
	writeContractTree(t, current, "package contract\n\ntype OldNote struct{}\n", nil)

	err := compareContractTrees(generated, current)
	if err == nil || !strings.Contains(err.Error(), "contract/contract.gen.go differs") {
		t.Errorf("compareContractTrees() = %v", err)
	}
}

func TestCompareContractTreesReportsMissingGoFile(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated")
	current := filepath.Join(root, "current")

	writeContractTree(t, generated, "package contract\n", nil)

	if err := os.MkdirAll(current, privateDirMode); err != nil {
		t.Fatal(err)
	}

	err := compareContractTrees(generated, current)
	if err == nil || !strings.Contains(err.Error(), "contract/contract.gen.go is missing") {
		t.Errorf("compareContractTrees() = %v", err)
	}
}

func TestRunContractCheckSkipsWithoutContract(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := runContract([]string{"--dir", dir, "--check"}); err != nil {
		t.Fatalf("runContract --check = %v", err)
	}
}

func TestCheckClientSkipsWithoutRegistry(t *testing.T) {
	dir := t.TempDir()

	if err := checkClient(dir, "web", dir); err != nil {
		t.Errorf("no registry and no client should pass: %v", err)
	}
}

func TestCheckClientFlagsStaleOutput(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "web", "src", "lib")

	if err := os.MkdirAll(lib, privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(lib, "api.gen.ts"), []byte("// stale"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := checkClient(dir, "web", dir); err == nil {
		t.Error("client output without a registry should fail")
	}
}

func TestCheckGeneratedComparesOutput(t *testing.T) {
	checks := []struct {
		check func(dir, web, generated string) error
		name  string
	}{
		{checkClient, "api.gen.ts"},
		{checkFixtures, "fixtures.gen.ts"},
	}

	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			dir := t.TempDir()
			generated := t.TempDir()
			lib := filepath.Join(dir, "web", "src", "lib")

			if err := os.MkdirAll(lib, privateDirMode); err != nil {
				t.Fatal(err)
			}

			if err := os.MkdirAll(filepath.Join(dir, "contract"), privateDirMode); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, "contract", "endpoints.ts"), []byte("export const endpoints = []"), privateFileMode); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(generated, check.name), []byte("// fresh"), privateFileMode); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(lib, check.name), []byte("// stale"), privateFileMode); err != nil {
				t.Fatal(err)
			}

			if err := check.check(dir, "web", generated); err == nil {
				t.Errorf("differing %s should fail", check.name)
			}

			if err := os.WriteFile(filepath.Join(lib, check.name), []byte("// fresh"), privateFileMode); err != nil {
				t.Fatal(err)
			}

			if err := check.check(dir, "web", generated); err != nil {
				t.Errorf("matching %s should pass: %v", check.name, err)
			}
		})
	}
}

func TestCheckFixturesSkipsWithoutRegistry(t *testing.T) {
	dir := t.TempDir()

	if err := checkFixtures(dir, "web", dir); err != nil {
		t.Errorf("no registry and no fixtures should pass: %v", err)
	}
}

func TestCheckFixturesFlagsStaleOutput(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "web", "src", "lib")

	if err := os.MkdirAll(lib, privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(lib, "fixtures.gen.ts"), []byte("// stale"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := checkFixtures(dir, "web", dir); err == nil {
		t.Error("fixtures output without a registry should fail")
	}
}
