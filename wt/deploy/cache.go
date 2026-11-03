package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const buildCacheName = ".wt-build.json"

// buildCache records the input hash and output digest of each build phase. A phase whose hash and output are unchanged is reused.
type buildCache struct {
	ComponentHash string `json:"componentHash,omitempty"`
	StaticHash    string `json:"staticHash,omitempty"`
	WasmDigest    string `json:"wasmDigest,omitempty"`
}

// writeBuildCache records a successful phase. Write errors are ignored because the cache only skips future work.
func writeBuildCache(dir string, cache buildCache) {
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}

	_ = os.WriteFile(filepath.Join(dir, buildCacheName), append(data, '\n'), privateFileMode)
}

// readBuildCache loads the last phase cache. A missing or malformed file reads as an empty cache, which forces a rebuild.
func readBuildCache(dir string) (buildCache, error) {
	var cache buildCache

	data, err := os.ReadFile(filepath.Join(dir, buildCacheName))
	if os.IsNotExist(err) {
		return cache, nil
	}

	if err != nil {
		return cache, err
	}

	if err := json.Unmarshal(data, &cache); err != nil {
		return buildCache{}, nil
	}

	return cache, nil
}

// commonSkipDir holds dependency, VCS and output trees that never change a component or static build.
func commonSkipDir(entry os.DirEntry) bool {
	switch entry.Name() {
	case ".git", "node_modules", "dist", "target", ".svelte-kit":
		return true
	}

	return false
}

// appComponentSkip excludes frontend output, dependencies and generated SDK bindings from the app's component inputs.
func appComponentSkip(root string) func(string, os.DirEntry) bool {
	return func(path string, entry os.DirEntry) bool {
		if commonSkipDir(entry) {
			return true
		}

		if entry.Name() == "testdata" {
			return true
		}

		if filepath.Dir(path) == root {
			switch entry.Name() {
			case "web", "static":
				return true
			}
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return false
		}

		switch filepath.ToSlash(rel) {
		case "sdk/binding", "sdk/tmp":
			return true
		}

		return false
	}
}

// moduleSkipDir excludes generated bindings and test fixtures inside a replaced local module.
func moduleSkipDir(root string) func(string, os.DirEntry) bool {
	return func(path string, entry os.DirEntry) bool {
		if commonSkipDir(entry) {
			return true
		}

		if entry.Name() == "testdata" {
			return true
		}

		if filepath.Dir(path) != root {
			return false
		}

		base := filepath.Base(root)
		if base != "packages" && base != "sdk" {
			return false
		}

		switch entry.Name() {
		case "binding", "tmp":
			return true
		}

		return false
	}
}

// staticSkipDir excludes SvelteKit and Vite output while keeping the frontend source, lockfile and static assets.
func staticSkipDir(root string) func(string, os.DirEntry) bool {
	return func(path string, entry os.DirEntry) bool {
		if commonSkipDir(entry) {
			return true
		}

		if filepath.Dir(path) == root {
			switch entry.Name() {
			case "build", ".vite":
				return true
			}
		}

		return false
	}
}

// walkFiles collects regular files under root, applying directory and file filters.
func walkFiles(root string, skipDir func(string, os.DirEntry) bool, include func(string, os.DirEntry) bool) ([]string, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		if entry.IsDir() {
			if path != root && skipDir != nil && skipDir(path, entry) {
				return filepath.SkipDir
			}

			return nil
		}

		if include != nil && !include(path, entry) {
			return nil
		}

		files = append(files, path)

		return nil
	})

	return files, err
}

// isComponentSource keeps the files that can change a compiled Go component. Generated Go and tests are handled through their sources.
func isComponentSource(_ string, entry os.DirEntry) bool {
	name := entry.Name()
	if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".gen.go") {
		return false
	}

	switch name {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}

	switch filepath.Ext(name) {
	case ".go", ".wit":
		return true
	}

	return false
}

// isModuleSource keeps the build files of a replaced local module, excluding tests and generated Go.
func isModuleSource(_ string, entry os.DirEntry) bool {
	name := entry.Name()
	if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".gen.go") {
		return false
	}

	switch name {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}

	switch filepath.Ext(name) {
	case ".go", ".s", ".wit", ".sh":
		return true
	}

	return false
}

// goModuleRoot finds the nearest go.mod above dir, so a monorepo app hashes the module and its replace targets, not just its own folder.
func goModuleRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	for {
		info, err := os.Stat(filepath.Join(abs, "go.mod"))
		if err == nil && !info.IsDir() {
			return abs, nil
		}

		if err != nil && !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(abs)
		if parent == abs {
			return "", nil
		}

		abs = parent
	}
}

// localReplaceDirs returns absolute local paths from the module's go.mod replace directives, so the shared SDK cannot fall outside a cache key.
func localReplaceDirs(dir string) ([]string, error) {
	root, err := goModuleRoot(dir)
	if err != nil || root == "" {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var paths []string

	inBlock := false

	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if index := strings.Index(line, "//"); index >= 0 {
			line = strings.TrimSpace(line[:index])
		}

		if line == "" {
			continue
		}

		if line == "replace (" {
			inBlock = true
			continue
		}

		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
		} else if !strings.HasPrefix(line, "replace ") {
			continue
		}

		line = strings.TrimPrefix(line, "replace ")

		_, right, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}

		fields := strings.Fields(right)
		if len(fields) == 0 {
			continue
		}

		value := fields[0]
		if strings.HasPrefix(value, ".") || filepath.IsAbs(value) {
			if !filepath.IsAbs(value) {
				value = filepath.Join(root, value)
			}

			paths = append(paths, value)
		}
	}

	return paths, nil
}

// dedupePaths sorts paths and removes duplicates so a walk and an explicit input can overlap.
func dedupePaths(paths []string) []string {
	seen := map[string]struct{}{}

	out := make([]string, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = filepath.Clean(path)
		}

		if _, ok := seen[abs]; ok {
			continue
		}

		seen[abs] = struct{}{}
		out = append(out, abs)
	}

	sort.Strings(out)

	return out
}

// collectComponentInputFiles walks Go, WIT and module files plus explicit component_inputs and local replace targets.
func collectComponentInputFiles(dir string, m manifest) ([]string, error) {
	files, err := walkFiles(dir, appComponentSkip(dir), isComponentSource)
	if err != nil {
		return nil, err
	}

	for _, value := range m.ComponentInputs {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}

		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			files = append(files, path)
			continue
		}

		more, err := walkFiles(path, moduleSkipDir(path), nil)
		if err != nil {
			return nil, err
		}

		files = append(files, more...)
	}

	replaces, err := localReplaceDirs(dir)
	if err != nil {
		return nil, err
	}

	root, err := goModuleRoot(dir)
	if err != nil {
		return nil, err
	}

	if root != "" {
		appAbs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}

		if root != appAbs {
			for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
				path := filepath.Join(root, name)
				if info, err := os.Stat(path); err == nil && !info.IsDir() {
					files = append(files, path)
				}
			}
		}
	}

	for _, value := range replaces {
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}

		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			files = append(files, path)
			continue
		}

		more, err := walkFiles(path, moduleSkipDir(path), isModuleSource)
		if err != nil {
			return nil, err
		}

		files = append(files, more...)
	}

	return dedupePaths(files), nil
}

// webDirName returns the SvelteKit source directory, defaulting to web.
func webDirName(m manifest) string {
	if m.Dev != nil && strings.TrimSpace(m.Dev.Web) != "" {
		return strings.TrimSpace(m.Dev.Web)
	}

	return "web"
}

// collectStaticInputFiles walks the frontend source and explicit static_inputs, excluding build output and dependencies.
func collectStaticInputFiles(dir string, m manifest) ([]string, error) {
	var files []string

	web := filepath.Join(dir, webDirName(m))
	if info, err := os.Stat(web); err == nil && info.IsDir() {
		more, err := walkFiles(web, staticSkipDir(web), func(_ string, entry os.DirEntry) bool {
			return entry.Name() != ".wt-dev.vite.config.ts"
		})
		if err != nil {
			return nil, err
		}

		files = append(files, more...)
	}

	for _, value := range m.StaticInputs {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}

		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			files = append(files, path)
			continue
		}

		more, err := walkFiles(path, staticSkipDir(path), nil)
		if err != nil {
			return nil, err
		}

		files = append(files, more...)
	}

	return dedupePaths(files), nil
}

// inputHash hashes a command, a phase seed and every input file by relative path and contents.
func inputHash(dir, command, seed string, files []string) (string, error) {
	hash := sha256.New()
	_, _ = io.WriteString(hash, "command\x00"+command+"\x00seed\x00"+seed+"\x00")

	base, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	for _, path := range files {
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return "", err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}

		_, _ = io.WriteString(hash, filepath.ToSlash(rel)+"\x00")
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// componentInputHash keys the component phase on Go, WIT, the replaced SDK and the component command, never the frontend.
func componentInputHash(dir string, m manifest, command string) (string, error) {
	files, err := collectComponentInputFiles(dir, m)
	if err != nil {
		return "", err
	}

	seed := "component\x00" + strings.Join(m.ComponentInputs, "\x00")

	return inputHash(dir, command, seed, files)
}

// staticInputHash keys the static phase on the frontend source, lockfile, config and the static command.
func staticInputHash(dir string, m manifest, command string) (string, error) {
	files, err := collectStaticInputFiles(dir, m)
	if err != nil {
		return "", err
	}

	seed := "static\x00" + webDirName(m) + "\x00" + strings.Join(m.StaticInputs, "\x00")

	return inputHash(dir, command, seed, files)
}
