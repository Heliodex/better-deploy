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
	"strings"
)

//go:embed testdata/contract/contract.ts
var contractScript string

//go:embed testdata/contract/client.ts
var clientScript string

//go:embed testdata/contract/fixtures.ts
var fixturesScript string

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
	fixturesOut := filepath.Join(webDir, "src", "lib", "fixtures.gen.ts")
	registry := filepath.Join(dir, "contract", "endpoints.ts")

	if outDir != filepath.Join(dir, "contract") {
		// Check mode: emit beside the real client so Biome resolves the same config and dependencies, then move the results over for comparison.
		if err := stageCheckOutput(webDir, outDir, "api.gen.ts", func(staged string) error {
			return generateClient(registry, webDir, staged, quiet)
		}); err != nil {
			return err
		}

		return stageCheckOutput(webDir, outDir, "fixtures.gen.ts", func(staged string) error {
			return generateFixtures(registry, webDir, staged, quiet)
		})
	}

	if err := generateClient(registry, webDir, clientOut, quiet); err != nil {
		return err
	}

	return generateFixtures(registry, webDir, fixturesOut, quiet)
}

// stageCheckOutput generates a file beside the checked-in sources and moves it into the comparison directory.
func stageCheckOutput(webDir, outDir, name string, generate func(staged string) error) error {
	staged, err := os.CreateTemp(filepath.Join(webDir, "src", "lib"), ".wt-check-*.gen.ts")
	if err != nil {
		return err
	}

	stagedPath := staged.Name()
	staged.Close()
	os.Remove(stagedPath)

	if err := generate(stagedPath); err != nil {
		os.Remove(stagedPath)
		return err
	}

	if err := os.Rename(stagedPath, filepath.Join(outDir, name)); err != nil {
		os.Remove(stagedPath)
		return err
	}

	return nil
}

// generateClient emits the typed fetch client when the app keeps an endpoint registry, and skips silently without one.
func generateClient(registry, webDir, outPath string, quiet bool) error {
	return generateScript(registry, clientEmitter, webDir, outPath, quiet)
}

// generateFixtures emits the fake factories when the app keeps an endpoint registry, and skips silently without one.
func generateFixtures(registry, webDir, outPath string, quiet bool) error {
	return generateScript(registry, fixturesEmitter, webDir, outPath, quiet)
}

// generateRegistryOutputs regenerates the fetch client and fake fixtures when the app keeps an endpoint registry.
func generateRegistryOutputs(dir, webDir string) error {
	registry := filepath.Join(dir, "contract", "endpoints.ts")

	if err := generateClient(registry, webDir, filepath.Join(webDir, "src", "lib", "api.gen.ts"), false); err != nil {
		return err
	}

	return generateFixtures(registry, webDir, filepath.Join(webDir, "src", "lib", "fixtures.gen.ts"), false)
}

// generateScript runs one embedded generator unless the app keeps no endpoint registry.
func generateScript(registry string, emitter contractEmitter, webDir, outPath string, quiet bool) error {
	if _, err := os.Stat(registry); os.IsNotExist(err) {
		return nil
	}

	return emitter.run(webDir, outPath, quiet)
}

// contractEmitter describes one embedded generator script and the environment it reads.
type contractEmitter struct {
	name     string
	pattern  string
	script   string
	outEnv   string
	quietEnv string
}

var (
	clientEmitter   = contractEmitter{name: "client", pattern: ".wt-client-*.ts", script: clientScript, outEnv: "WT_CLIENT_OUT", quietEnv: "WT_CLIENT_QUIET"}
	fixturesEmitter = contractEmitter{name: "fixtures", pattern: ".wt-fixtures-*.ts", script: fixturesScript, outEnv: "WT_FIXTURES_OUT", quietEnv: "WT_FIXTURES_QUIET"}
)

// run executes one embedded generator from the web directory so its imports resolve.
func (e contractEmitter) run(webDir, outPath string, quiet bool) error {
	env := []string{e.outEnv + "=" + outPath}
	if quiet {
		env = append(env, e.quietEnv+"=1")
	}

	if err := runBunScript(webDir, e.pattern, e.script, env...); err != nil {
		return fmt.Errorf("contract %s: %w", e.name, err)
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

	var output strings.Builder

	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Run(); err != nil {
		// A project that exempts generated files from Biome (like the scaffold does) has nothing to format.
		if strings.Contains(output.String(), "No files were processed") {
			return nil
		}

		fmt.Fprint(os.Stderr, output.String())

		return fmt.Errorf("contract client format: %w", err)
	}

	if !quiet {
		fmt.Fprint(os.Stderr, output.String())
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
	env := []string{
		"WT_CONTRACT_OUT=" + filepath.Join(outDir, "schemas"),
		"WT_APP_NAME=" + appName,
	}
	if quiet {
		env = append(env, "WT_CONTRACT_QUIET=1")
	}

	if err := runBunScript(webDir, ".wt-contract-*.ts", contractScript, env...); err != nil {
		return fmt.Errorf("contract schemas: %w", err)
	}

	return nil
}

// runBunScript writes one embedded script beside the web sources, runs it with Bun, and removes it.
func runBunScript(webDir, pattern, script string, env ...string) error {
	file, err := os.CreateTemp(webDir, pattern)
	if err != nil {
		return err
	}

	scriptPath := file.Name()

	defer os.Remove(scriptPath)

	if _, err := file.WriteString(script); err != nil {
		file.Close()

		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), "bun", filepath.Base(scriptPath))
	cmd.Dir = webDir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
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
