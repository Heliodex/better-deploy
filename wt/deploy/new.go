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
)

//go:embed all:testdata/template
var templateFS embed.FS

type newConfig struct {
	dir string
}

func resolveNewConfig(args []string) (*newConfig, error) {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	dir := flags.String("dir", ".", "directory to scaffold the app in")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return &newConfig{dir: *dir}, nil
}

// exampleModule returns the Go module path for apps scaffolded directly under wt/example.
func exampleModule(dir string) (string, error) {
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != "example" {
		return "", fmt.Errorf("new apps must live directly under wt/example (got %s)", dir)
	}
	data, err := os.ReadFile(filepath.Join(parent, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read example go.mod: %w", err)
	}
	module := ""
	for _, line := range strings.Split(string(data), "\n") {
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
		return "", fmt.Errorf("no wt/packages next to wt/example (got %s)", dir)
	}
	return module, nil
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
	module, err := exampleModule(abs)
	if err != nil {
		return err
	}
	contractImport := module + "/" + name + "/contract"

	entries, err := os.ReadDir(abs)
	switch {
	case err == nil:
		if len(entries) > 0 {
			return fmt.Errorf("directory %s is not empty", cfg.dir)
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", cfg.dir, err)
		}
	default:
		return err
	}

	count := 0
	err = fs.WalkDir(templateFS, "testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "testdata/template/")
		dst := filepath.Join(abs, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		out := strings.ReplaceAll(string(data), newNameToken, name)
		out = strings.ReplaceAll(out, newContractToken, contractImport)
		mode := os.FileMode(0o644)
		if rel == "contract/generate.sh" {
			mode = 0o755
		}
		if err := os.WriteFile(dst, []byte(out), mode); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("new: created %s (%d files)\n", cfg.dir, count)
	fmt.Printf("next:\n  wt-deploy dev --dir %s\n", cfg.dir)
	fmt.Printf("  wt-deploy deploy --dir %s --host <supervisor> --app-host %s.<domain>\n", cfg.dir, name)
	return nil
}
