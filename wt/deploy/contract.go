package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

//go:embed testdata/contract/contract.ts
var contractScript string

// generateContract converts the app's Valibot registries to JSON Schema and generated Go types.
func generateContract(dir, web, appName string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	dir = abs

	schemas := filepath.Join(dir, web, "src", "lib", "contract.ts")
	if _, err := os.Stat(schemas); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("app has no %s/src/lib/contract.ts; scaffold with --contract=valibot or use hand-written types", web)
		}

		return err
	}

	if _, err := exec.LookPath("bun"); err != nil {
		return fmt.Errorf("contract generation needs bun: %w", err)
	}

	webDir := filepath.Join(dir, web)
	if err := ensureWebDependencies(webDir); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(dir, "contract"), privateDirMode); err != nil {
		return err
	}

	if err := runContractSchemas(dir, webDir, appName); err != nil {
		return err
	}

	return runGoJSONSchema(dir)
}

// ensureWebDependencies installs the frontend packages when node_modules is missing.
func ensureWebDependencies(webDir string) error {
	if info, err := os.Stat(filepath.Join(webDir, "node_modules")); err == nil && info.IsDir() {
		return nil
	}

	cmd := exec.CommandContext(context.Background(), "bun", "install")
	cmd.Dir = webDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bun install: %w", err)
	}

	return nil
}

// runContractSchemas runs the embedded converter from the web directory so its imports resolve.
func runContractSchemas(dir, webDir, appName string) error {
	file, err := os.CreateTemp(webDir, ".wt-contract-*.ts")
	if err != nil {
		return err
	}

	script := file.Name()

	defer os.Remove(script)

	if _, err := file.WriteString(contractScript); err != nil {
		file.Close()

		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), "bun", filepath.Base(script))
	cmd.Dir = webDir

	cmd.Env = append(os.Environ(),
		"WT_CONTRACT_OUT="+filepath.Join(dir, "contract", "schemas"),
		"WT_APP_NAME="+appName,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("contract schemas: %w", err)
	}

	return nil
}

// runGoJSONSchema turns the generated schema files into contract/contract.gen.go.
func runGoJSONSchema(dir string) error {
	contractDir := filepath.Join(dir, "contract")

	schemas, err := filepath.Glob(filepath.Join(contractDir, "schemas", "*.json"))
	if err != nil {
		return err
	}

	if len(schemas) == 0 {
		return errors.New("contract schemas: no JSON Schema files were generated")
	}

	sort.Strings(schemas)

	args := []string{
		"tool", "go-jsonschema",
		"--package", "contract",
		"--struct-name-from-title",
		"--capitalization", "ID",
		"--capitalization", "URL",
		"--capitalization", "MS",
		"--capitalization", "OK",
		"-o", "contract.gen.go",
	}
	for _, path := range schemas {
		args = append(args, filepath.Join("schemas", filepath.Base(path)))
	}

	cmd := exec.CommandContext(context.Background(), "go", args...)
	cmd.Dir = contractDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go-jsonschema: %w", err)
	}

	return nil
}
