package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Template placeholders substituted when scaffolding. Templates must not use these tokens otherwise.
const (
	newNameToken       = "__APP_NAME__"        //nolint:gosec // Template placeholder, not a credential.
	newContractToken   = "__CONTRACT_IMPORT__" //nolint:gosec // Template placeholder, not a credential.
	newModuleToken     = "__APP_MODULE__"      //nolint:gosec // Template placeholder, not a credential.
	newSDKPathToken    = "__SDK_MODULE__"      //nolint:gosec // Template placeholder, not a credential.
	newSDKVersionToken = "__SDK_VERSION__"     //nolint:gosec // Template placeholder, not a credential.
	newSDKPrepareToken = "__SDK_PREPARE__"     //nolint:gosec // Template placeholder, not a credential.
	newSDKWITToken     = "__SDK_WIT__"         //nolint:gosec // Template placeholder, not a credential.
)

// The SDK module is published from wt/packages with tags such as wt/packages/v0.1.0.
const (
	sdkModulePath    = "github.com/heliodex/better-deploy/wt/packages"
	sdkModuleVersion = "v0.1.0"
)

// Template files embedded into the binary. The template's web/node_modules and web/bun.lock are deliberately omitted so a new app starts without them. Keep this list in sync with testdata/template.
//
//go:embed all:testdata/template/app.json
//go:embed all:testdata/template/biome.json
//go:embed all:testdata/template/.oxlintrc.json
//go:embed all:testdata/template/main.go
//go:embed all:testdata/template/main_test.go
//go:embed all:testdata/template/wasm.go
//go:embed all:testdata/template/.gitignore
//go:embed all:testdata/template/contract
//go:embed all:testdata/template/web/package.json
//go:embed all:testdata/template/web/tsconfig.json
//go:embed all:testdata/template/web/vite.config.ts
//go:embed all:testdata/template/web/.npmrc
//go:embed all:testdata/template/web/src
//go:embed all:testdata/template/web/scripts
//go:embed all:testdata/template/web/static
var templateFS embed.FS

//go:embed all:testdata/standalone
var standaloneFS embed.FS

//go:embed all:testdata/contractless
var contractlessFS embed.FS

type newConfig struct {
	dir, module, sdk, contract string
}

func resolveNewConfig(args []string) (*newConfig, error) {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	dir := flags.String("dir", ".", "directory to scaffold the app in")
	module := flags.String("module", "", "Go module path for a standalone app (default: app name)")

	sdk := flags.String("sdk", "", "replace the published Go SDK with a local wt/packages checkout")

	contract := flags.String("contract", "valibot", "contract pipeline: valibot or none")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	switch *contract {
	case "valibot", "none":
	default:
		return nil, fmt.Errorf("--contract must be valibot or none, got %q", *contract)
	}

	sdkPath := ""

	if *sdk != "" {
		abs, err := filepath.Abs(*sdk)
		if err != nil {
			return nil, err
		}

		sdkPath = abs
	}

	return &newConfig{dir: *dir, module: *module, sdk: sdkPath, contract: *contract}, nil
}

type scaffoldMode struct {
	monorepo   bool
	module     string
	contract   string
	sdkPrepare string
	sdkWIT     string
	standalone bool
}

// exampleModule returns the Go module path for apps scaffolded directly under wt/example.
func exampleModule(dir string) (string, error) {
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != "example" {
		return "", errors.New("not under wt/example")
	}

	data, err := os.ReadFile(filepath.Join(parent, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read example go.mod: %w", err)
	}

	module := ""

	for line := range strings.SplitSeq(string(data), "\n") {
		name, ok := strings.CutPrefix(line, "module ")
		if !ok {
			continue
		}

		module = strings.TrimSpace(name)

		break
	}

	if module == "" {
		return "", errors.New("example go.mod has no module line")
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "packages", "go.mod")); err != nil {
		return "", errors.New("no wt/packages next to wt/example")
	}

	return module, nil
}

// scaffoldFor chooses the monorepo layout for apps under wt/example and a standalone layout otherwise.
func scaffoldFor(cfg *newConfig, abs, name string) (scaffoldMode, error) {
	if cfg.sdk == "" {
		if module, err := exampleModule(abs); err == nil {
			return scaffoldMode{
				monorepo:   true,
				module:     module,
				contract:   module + "/" + name + "/contract",
				sdkPrepare: "../../packages/generate.sh && ",
				sdkWIT:     "../../packages/wit",
			}, nil
		}
	}

	module := strings.TrimSpace(cfg.module)
	if module == "" {
		module = name
	}

	if strings.ContainsAny(module, " \t\r\n") {
		return scaffoldMode{}, fmt.Errorf("module %q contains whitespace", module)
	}

	return scaffoldMode{
		monorepo:   false,
		module:     module,
		contract:   module + "/contract",
		sdkPrepare: "",
		sdkWIT:     `\"$(go list -m -f '{{.Dir}}' ` + sdkModulePath + `)/wit\"`,
		standalone: true,
	}, nil
}

// writeStandaloneGoMod writes the app go.mod, requiring the published SDK or a local replace.
func writeStandaloneGoMod(abs, module, sdkPath string) error {
	data, err := standaloneFS.ReadFile("testdata/standalone/go.mod.tmpl")
	if err != nil {
		return err
	}

	text := strings.ReplaceAll(string(data), newModuleToken, module)
	text = strings.ReplaceAll(text, newSDKPathToken, sdkModulePath)
	text = strings.ReplaceAll(text, newSDKVersionToken, sdkModuleVersion)

	if sdkPath != "" {
		text += "\nreplace " + sdkModulePath + " => " + filepath.ToSlash(sdkPath) + "\n"
	}

	return os.WriteFile(filepath.Join(abs, "go.mod"), []byte(text), scaffoldFileMode)
}

// tidyStandaloneModule resolves the SDK and tool dependencies into go.mod and go.sum.
func tidyStandaloneModule(abs string) {
	cmd := exec.CommandContext(context.Background(), "go", "mod", "tidy")
	cmd.Dir = abs
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "new: go mod tidy failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "new: run `go mod tidy` in %s later; for offline work, scaffold with --sdk PATH to use a local SDK checkout\n", abs)
	}
}

// prepareNewDir creates the target if needed and reports whether it already contains files.
func prepareNewDir(display, abs string) (bool, error) {
	entries, err := os.ReadDir(abs)
	switch {
	case err == nil:
		return len(entries) > 0, nil
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, privateDirMode); err != nil {
			return false, fmt.Errorf("create %s: %w", display, err)
		}

		return false, nil
	default:
		return false, err
	}
}

// writeTemplateFiles walks the embedded template and substitutes the app name, contract and SDK paths.
func writeTemplateFiles(abs, name string, mode scaffoldMode) (int, error) {
	count := 0
	err := fs.WalkDir(templateFS, "testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, ok := strings.CutPrefix(path, "testdata/template")
		if !ok {
			return fmt.Errorf("unexpected template path %q", path)
		}

		rel = strings.TrimPrefix(rel, "/")

		dst := filepath.Join(abs, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(dst, privateDirMode)
		}

		data, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}

		out := strings.ReplaceAll(string(data), newNameToken, name)
		out = strings.ReplaceAll(out, newContractToken, mode.contract)
		out = strings.ReplaceAll(out, newSDKPrepareToken, mode.sdkPrepare)

		out = strings.ReplaceAll(out, newSDKWITToken, mode.sdkWIT)
		if err := os.WriteFile(dst, []byte(out), scaffoldFileMode); err != nil {
			return err
		}

		count++

		return nil
	})

	return count, err
}

// validateLocalSDK accepts only a packages directory that declares the SDK module.
func validateLocalSDK(dir string) error {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	if err != nil || info.IsDir() {
		return fmt.Errorf("--sdk %s is not a packages directory with go.mod", dir)
	}

	module, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil || !strings.Contains(string(module), "module "+sdkModulePath) {
		return fmt.Errorf("--sdk %s does not contain module %s", dir, sdkModulePath)
	}

	return nil
}

// writeContractlessScaffold replaces the generated-contract wiring with hand-written note types and removes the generator.
func writeContractlessScaffold(abs string) (int, error) {
	for _, name := range []string{"main.go", "main_test.go"} {
		data, err := contractlessFS.ReadFile("testdata/contractless/" + name)
		if err != nil {
			return 0, err
		}

		if err := os.WriteFile(filepath.Join(abs, name), data, scaffoldFileMode); err != nil {
			return 0, err
		}
	}

	if err := os.RemoveAll(filepath.Join(abs, "contract")); err != nil {
		return 0, err
	}

	if err := os.Remove(filepath.Join(abs, "web", "scripts", "contract.ts")); err != nil && !os.IsNotExist(err) {
		return 0, err
	}

	_ = os.Remove(filepath.Join(abs, "web", "scripts"))

	manifestPath := filepath.Join(abs, "app.json")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return 0, err
	}

	text := strings.ReplaceAll(string(data), "go generate ./contract/ && ", "")
	text = strings.ReplaceAll(text, `, "web/src/lib/contract.ts"`, "")
	text = strings.ReplaceAll(text, `["web/src/lib/contract.ts"]`, "[]")

	if err := os.WriteFile(manifestPath, []byte(text), scaffoldFileMode); err != nil {
		return 0, err
	}

	contractPath := filepath.Join(abs, "web", "src", "lib", "contract.ts")

	contractData, err := os.ReadFile(contractPath)
	if err != nil {
		return 0, err
	}

	contractText := strings.Replace(string(contractData), "// These schemas are the source of truth for the generated Go contract. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.", "// These schemas validate requests and responses in the browser. The Go notes types are hand-written in main.go.", 1)
	if err := os.WriteFile(contractPath, []byte(contractText), scaffoldFileMode); err != nil {
		return 0, err
	}

	return 2, nil
}

// printNewSummary shows what was created and the first commands to run.
func printNewSummary(dir, name string, mode scaffoldMode, count int, sdkPath string) {
	layout := map[bool]string{true: "standalone", false: "monorepo"}[mode.standalone]
	fmt.Printf("new: created %s (%d files, %s)\n", dir, count, layout)
	fmt.Printf("next:\n  wt-deploy dev --dir %s\n", dir)
	fmt.Printf("  wt-deploy deploy --dir %s --host <supervisor> --app-host %s.<domain>\n", dir, name)

	if mode.standalone {
		fmt.Printf("  module: %s\n", mode.module)

		if sdkPath != "" {
			fmt.Printf("  sdk: %s (local replace)\n", sdkPath)
		} else {
			fmt.Printf("  sdk: %s %s\n", sdkModulePath, sdkModuleVersion)
		}
	}
}

// scaffoldNew writes the app files into cfg.dir and returns the chosen layout and absolute directory.
func scaffoldNew(cfg *newConfig) (scaffoldMode, string, error) {
	abs, err := filepath.Abs(cfg.dir)
	if err != nil {
		return scaffoldMode{}, "", err
	}

	name := filepath.Base(abs)
	if !validName(name) {
		return scaffoldMode{}, "", fmt.Errorf("invalid app name %q: use lowercase letters, digits and hyphens", name)
	}

	mode, err := scaffoldFor(cfg, abs, name)
	if err != nil {
		return scaffoldMode{}, "", err
	}

	nonEmpty, err := prepareNewDir(cfg.dir, abs)
	if err != nil {
		return scaffoldMode{}, "", err
	}

	if nonEmpty {
		fmt.Fprintf(os.Stderr, "new: %s is not empty; overwriting matching files and leaving other files in place\n", cfg.dir)
	}

	count, err := writeTemplateFiles(abs, name, mode)
	if err != nil {
		return scaffoldMode{}, "", err
	}

	if cfg.contract == "none" {
		contractCount, err := writeContractlessScaffold(abs)
		if err != nil {
			return scaffoldMode{}, "", err
		}

		count += contractCount
	}

	if mode.standalone {
		if cfg.sdk != "" {
			if err := validateLocalSDK(cfg.sdk); err != nil {
				return scaffoldMode{}, "", err
			}
		}

		if err := writeStandaloneGoMod(abs, mode.module, cfg.sdk); err != nil {
			return scaffoldMode{}, "", err
		}

		count++
	}

	printNewSummary(cfg.dir, name, mode, count, cfg.sdk)

	return mode, abs, nil
}

func runNew(args []string) error {
	cfg, err := resolveNewConfig(args)
	if err != nil {
		return err
	}

	mode, abs, err := scaffoldNew(cfg)
	if err != nil {
		return err
	}

	if mode.standalone {
		tidyStandaloneModule(abs)
	}

	return nil
}
