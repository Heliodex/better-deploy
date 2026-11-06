package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

//go:embed testdata/contract/contract.ts
var contractScript string

//go:embed testdata/contract/client.ts
var clientScript string

// generateContract converts the app's Valibot registries to JSON Schema and generated Go types.
func generateContract(dir, web, appName string) error {
	return generateContractTo(dir, web, appName, "", false)
}

// generateContractTo writes the generated files to outDir, or the app's contract directory when outDir is empty.
func generateContractTo(dir, web, appName, outDir string, quiet bool) error {
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

	if outDir == "" {
		outDir = filepath.Join(dir, "contract")
	} else {
		outDir, err = filepath.Abs(outDir)
		if err != nil {
			return err
		}
	}

	if err := os.MkdirAll(outDir, privateDirMode); err != nil {
		return err
	}

	if err := runContractSchemas(webDir, appName, outDir, quiet); err != nil {
		return err
	}

	if err := runGoJSONSchema(outDir); err != nil {
		return err
	}

	clientOut := filepath.Join(webDir, "src", "lib", "api.gen.ts")
	if outDir != filepath.Join(dir, "contract") {
		// Check mode: emit beside the real client so Biome resolves the same config and dependencies, then move the result over for comparison.
		staged, err := os.CreateTemp(filepath.Join(webDir, "src", "lib"), ".wt-client-check-*.gen.ts")
		if err != nil {
			return err
		}

		stagedPath := staged.Name()
		staged.Close()
		os.Remove(stagedPath)

		if err := generateClient(filepath.Join(dir, "contract", "endpoints.ts"), webDir, stagedPath, quiet); err != nil {
			os.Remove(stagedPath)
			return err
		}

		if err := os.Rename(stagedPath, filepath.Join(outDir, "api.gen.ts")); err != nil {
			os.Remove(stagedPath)
			return err
		}

		return nil
	}

	return generateClient(filepath.Join(dir, "contract", "endpoints.ts"), webDir, clientOut, quiet)
}

// generateClient emits the typed fetch client when the app keeps an endpoint registry, and skips silently without one.
func generateClient(registry, webDir, outPath string, quiet bool) error {
	if _, err := os.Stat(registry); os.IsNotExist(err) {
		return nil
	}

	return runClientEmitter(webDir, outPath, quiet)
}

// runClientEmitter runs the embedded client generator from the web directory so its imports resolve.
func runClientEmitter(webDir, outPath string, quiet bool) error {
	file, err := os.CreateTemp(webDir, ".wt-client-*.ts")
	if err != nil {
		return err
	}

	script := file.Name()

	defer os.Remove(script)

	if _, err := file.WriteString(clientScript); err != nil {
		file.Close()

		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), "bun", filepath.Base(script))
	cmd.Dir = webDir

	env := append(os.Environ(), "WT_CLIENT_OUT="+outPath)
	if quiet {
		env = append(env, "WT_CLIENT_QUIET=1")
	}

	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("contract client: %w", err)
	}

	return formatGeneratedClient(webDir, outPath, quiet)
}

// formatGeneratedClient runs the project's Biome over the emitted client so the checked-in file always matches `bun lint`. Without a Biome config the raw emitter output stands, which stays deterministic for --check either way.
func formatGeneratedClient(webDir, outPath string, quiet bool) error {
	config := filepath.Join(webDir, "biome.json")
	if _, err := os.Stat(config); err != nil {
		config = filepath.Join(webDir, "biome.jsonc")
		if _, err := os.Stat(config); err != nil {
			return nil
		}
	}

	cmd := exec.CommandContext(context.Background(), "bunx", "biome", "check", "--fix", "--config-path="+config, outPath)
	cmd.Dir = webDir

	if quiet {
		cmd.Stdout = io.Discard
	} else {
		cmd.Stdout = os.Stderr
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("contract client format: %w", err)
	}

	return nil
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
func runContractSchemas(webDir, appName, outDir string, quiet bool) error {
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

	env := append(os.Environ(),
		"WT_CONTRACT_OUT="+filepath.Join(outDir, "schemas"),
		"WT_APP_NAME="+appName,
	)
	if quiet {
		env = append(env, "WT_CONTRACT_QUIET=1")
	}

	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("contract schemas: %w", err)
	}

	return nil
}

// runGoJSONSchema turns the generated schema files into contract.gen.go in contractDir.
func runGoJSONSchema(contractDir string) error {
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
