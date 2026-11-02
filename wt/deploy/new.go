package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Template placeholders substituted when scaffolding. Templates must not use these tokens otherwise.
const (
	newNameToken     = "__APP_NAME__"
	newContractToken = "__CONTRACT_IMPORT__"
	newModuleToken   = "__APP_MODULE__"
	newSDKGenToken   = "__SDK_GENERATE__"
	newSDKWITToken   = "__SDK_WIT__"
)

// Template files embedded into the binary. Listed explicitly so the template's own web/node_modules and web/bun.lock are never embedded, and therefore never copied into new apps. Run bun install there to materialize them instead. Keep this list in sync with testdata/template (TestNewMatchesTemplate fails if a template file is missing here).
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

//go:embed all:testdata/sdk
var sdkFS embed.FS

//go:embed all:testdata/standalone
var standaloneFS embed.FS

type newConfig struct {
	dir, module, sdk string
}

func resolveNewConfig(args []string) (*newConfig, error) {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	dir := flags.String("dir", ".", "directory to scaffold the app in")
	module := flags.String("module", "", "Go module path for a standalone app (default: app name)")
	sdk := flags.String("sdk", "", "copy the Go SDK from this packages directory instead of the embedded SDK")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return &newConfig{dir: *dir, module: *module, sdk: *sdk}, nil
}

type scaffoldMode struct {
	monorepo      bool
	module        string
	contract      string
	sdkGenerate   string
	sdkWIT        string
	standalone    bool
	writeGoModule bool
}

// exampleModule returns the Go module path for apps scaffolded directly under wt/example.
func exampleModule(dir string) (string, error) {
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != "example" {
		return "", fmt.Errorf("not under wt/example")
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
		return "", fmt.Errorf("example go.mod has no module line")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "packages", "go.mod")); err != nil {
		return "", fmt.Errorf("no wt/packages next to wt/example")
	}
	return module, nil
}

// scaffoldFor chooses the monorepo layout for apps under wt/example and a standalone layout otherwise.
func scaffoldFor(cfg *newConfig, abs, name string) (scaffoldMode, error) {
	if cfg.sdk == "" {
		if module, err := exampleModule(abs); err == nil {
			return scaffoldMode{
				monorepo:    true,
				module:      module,
				contract:    module + "/" + name + "/contract",
				sdkGenerate: "../../packages/generate.sh",
				sdkWIT:      "../../packages/wit",
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
		monorepo:      false,
		module:        module,
		contract:      module + "/contract",
		sdkGenerate:   "sh ./sdk/generate.sh",
		sdkWIT:        "./sdk/wit",
		standalone:    true,
		writeGoModule: true,
	}, nil
}

// copyEmbeddedDir copies the embedded SDK tree, preserving executable bits on shell scripts.
func copyEmbeddedDir(root, dst string) (int, error) {
	count := 0
	err := fs.WalkDir(sdkFS, root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(path, root)
		if !ok {
			return nil
		}
		rel = strings.TrimPrefix(rel, "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := sdkFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// copyLocalDir copies a checked-out SDK, skipping generated bindings, temp files and tests.
func copyLocalDir(src, dst string) (int, error) {
	count := 0
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && (entry.Name() == "binding" || entry.Name() == "tmp") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, err := entry.Info(); err == nil && info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// writeStandaloneGoMod writes the app's go.mod and go.sum plus the SDK's module files for a standalone scaffold.
func writeStandaloneGoMod(abs, module string) error {
	data, err := standaloneFS.ReadFile("testdata/standalone/go.mod.tmpl")
	if err != nil {
		return err
	}
	text := strings.ReplaceAll(string(data), newModuleToken, module)
	if err := os.WriteFile(filepath.Join(abs, "go.mod"), []byte(text), 0o644); err != nil {
		return err
	}
	sum, err := standaloneFS.ReadFile("testdata/standalone/go.sum")
	if err == nil {
		if err := os.WriteFile(filepath.Join(abs, "go.sum"), sum, 0o644); err != nil {
			return err
		}
	}
	sdkMod, err := standaloneFS.ReadFile("testdata/standalone/sdk.go.mod")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "sdk", "go.mod"), sdkMod, 0o644); err != nil {
		return err
	}
	sdkSum, err := standaloneFS.ReadFile("testdata/standalone/sdk.go.sum")
	if err == nil {
		if err := os.WriteFile(filepath.Join(abs, "sdk", "go.sum"), sdkSum, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prepareNewDir checks that the target is empty or creates it.
func prepareNewDir(display, abs string) error {
	entries, err := os.ReadDir(abs)
	switch {
	case err == nil:
		if len(entries) > 0 {
			return fmt.Errorf("directory %s is not empty", display)
		}
		return nil
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", display, err)
		}
		return nil
	default:
		return err
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
			return os.MkdirAll(dst, 0o755)
		}
		data, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		out := strings.ReplaceAll(string(data), newNameToken, name)
		out = strings.ReplaceAll(out, newContractToken, mode.contract)
		out = strings.ReplaceAll(out, newSDKGenToken, mode.sdkGenerate)
		out = strings.ReplaceAll(out, newSDKWITToken, mode.sdkWIT)
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
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
	if err != nil || !strings.Contains(string(module), "module wt.packages") {
		return fmt.Errorf("--sdk %s does not contain module wt.packages", dir)
	}
	return nil
}

// appendStandaloneIgnore keeps the copied SDK bindings and temp tree out of git.
func appendStandaloneIgnore(abs string) error {
	ignore, _ := os.ReadFile(filepath.Join(abs, ".gitignore"))
	if strings.Contains(string(ignore), "sdk/binding/") {
		return nil
	}
	appendIgnore := "sdk/binding/\nsdk/tmp/\n"
	if err := os.WriteFile(filepath.Join(abs, ".gitignore"), append(ignore, []byte(appendIgnore)...), 0o644); err != nil {
		return err
	}
	return nil
}

// installStandaloneSDK copies the SDK tree and rewrites the generated module file.
func installStandaloneSDK(cfg *newConfig, abs, module string) (int, error) {
	var sdkCount int
	var err error
	if cfg.sdk != "" {
		if err := validateLocalSDK(cfg.sdk); err != nil {
			return 0, err
		}
		sdkCount, err = copyLocalDir(cfg.sdk, filepath.Join(abs, "sdk"))
	} else {
		sdkCount, err = copyEmbeddedDir("testdata/sdk", filepath.Join(abs, "sdk"))
	}
	if err != nil {
		return 0, err
	}
	if err := writeStandaloneGoMod(abs, module); err != nil {
		return 0, err
	}
	if err := appendStandaloneIgnore(abs); err != nil {
		return 0, err
	}
	return sdkCount, nil
}

// printNewSummary shows what was created and the first commands to run.
func printNewSummary(dir, name string, mode scaffoldMode, count int) {
	layout := map[bool]string{true: "standalone", false: "monorepo"}[mode.standalone]
	fmt.Printf("new: created %s (%d files, %s)\n", dir, count, layout)
	fmt.Printf("next:\n  wt-deploy dev --dir %s\n", dir)
	fmt.Printf("  wt-deploy deploy --dir %s --host <supervisor> --app-host %s.<domain>\n", dir, name)
	if mode.standalone {
		fmt.Printf("  module: %s\n", mode.module)
	}
}

func runNew(args []string) error {
	cfg, err := resolveNewConfig(args)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(cfg.dir)
	if err != nil {
		return err
	}
	name := filepath.Base(abs)
	if !validName(name) {
		return fmt.Errorf("invalid app name %q: use lowercase letters, digits and hyphens", name)
	}
	mode, err := scaffoldFor(cfg, abs, name)
	if err != nil {
		return err
	}
	if err := prepareNewDir(cfg.dir, abs); err != nil {
		return err
	}
	count, err := writeTemplateFiles(abs, name, mode)
	if err != nil {
		return err
	}
	if mode.standalone {
		sdkCount, err := installStandaloneSDK(cfg, abs, mode.module)
		if err != nil {
			return err
		}
		count += sdkCount
	}
	printNewSummary(cfg.dir, name, mode, count)
	return nil
}
