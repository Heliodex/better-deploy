package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const buildCacheName = ".wt-build.json"

// buildCache records the input hash and output digest of each build phase. A phase whose hash and output are unchanged is reused.
type buildCache struct {
	ComponentHash string `json:"componentHash,omitempty"`
	StaticHash    string `json:"staticHash,omitempty"`
	WasmDigest    string `json:"wasmDigest,omitempty"`
}

// buildStatus reports which phases ran, which were reused and the resulting component digest.
type buildStatus struct {
	ComponentBuilt, ComponentReused, StaticBuilt, StaticReused, StaticSkipped, BuildSkipped bool
	WasmDigest                                                                              string
}

// writeBuildCache records a successful phase. Write errors are ignored because the cache only skips future work.
func writeBuildCache(dir string, cache buildCache) {
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}

	_ = os.WriteFile(filepath.Join(dir, buildCacheName), append(data, '\n'), 0o644)
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
func isComponentSource(path string, entry os.DirEntry) bool {
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
func isModuleSource(path string, entry os.DirEntry) bool {
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
		more, err := walkFiles(web, staticSkipDir(web), func(path string, entry os.DirEntry) bool {
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

// checkStaticSvelteKit rejects server-only SvelteKit files: the production artifact is adapter-static output.
func checkStaticSvelteKit(dir string, m manifest) error {
	web := webDirName(m)

	src := filepath.Join(dir, web, "src")
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return nil
	}

	var found []string

	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}

		name := entry.Name()
		rel, _ := filepath.Rel(dir, path)
		serverFile := strings.HasSuffix(name, ".server.ts") || strings.HasSuffix(name, ".server.js") || strings.HasSuffix(name, ".server.mjs") || strings.HasPrefix(name, "+server.")

		serverDir := strings.Contains(filepath.ToSlash(rel), "/lib/server/")
		if serverFile || serverDir {
			found = append(found, filepath.ToSlash(rel))
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("check SvelteKit routes: %w", err)
	}

	if len(found) > 0 {
		return fmt.Errorf("SvelteKit apps are static-only; remove server-only files or move the logic into the Go component: %s", strings.Join(found, ", "))
	}

	return nil
}

// prepareBuilds runs each stale phase, updates the cache after each success and reports what was reused.
func prepareBuilds(cfg *deployConfig, m manifest) (buildStatus, error) {
	var status buildStatus
	if err := checkStaticSvelteKit(cfg.dir, m); err != nil {
		return status, err
	}

	componentCmd := strings.TrimSpace(cfg.buildComponentCommand)
	if componentCmd == "" {
		componentCmd = strings.TrimSpace(m.BuildComponent)
	}

	staticCmd := strings.TrimSpace(m.BuildStatic)
	if cfg.skipStatic {
		staticCmd = ""
		status.StaticSkipped = true
	}

	if cfg.skipBuild {
		status.BuildSkipped = true
		if digest, err := fileDigest(filepath.Join(cfg.dir, "main.wasm")); err == nil {
			status.WasmDigest = digest
		}

		return status, nil
	}

	wasmPath := filepath.Join(cfg.dir, "main.wasm")
	wasmInfo, wasmErr := os.Stat(wasmPath)
	wasmExists := wasmErr == nil && wasmInfo.Mode().IsRegular()

	hasComponentBuild := componentCmd != ""
	if !hasComponentBuild && !wasmExists {
		return status, errors.New("app.json has no build_component and main.wasm is missing")
	}

	if !hasComponentBuild && cfg.forceBuild {
		return status, errors.New("cannot force-build without a component build command")
	}

	staticDir := filepath.Join(cfg.dir, "static")
	staticInfo, staticErr := os.Stat(staticDir)
	staticExists := staticErr == nil && staticInfo.IsDir()
	staticRelevant := staticCmd != ""

	cache, _ := readBuildCache(cfg.dir)

	var (
		componentHash, staticHash string
		err                       error
	)
	if hasComponentBuild {
		componentHash, err = componentInputHash(cfg.dir, m, componentCmd)
		if err != nil {
			return status, err
		}
	}

	if staticRelevant {
		staticHash, err = staticInputHash(cfg.dir, m, staticCmd)
		if err != nil {
			return status, err
		}
	}

	componentStale := false
	if hasComponentBuild {
		componentStale = cfg.forceBuild || cache.ComponentHash != componentHash || !wasmExists
	}

	staticStale := false
	if staticRelevant {
		staticStale = cfg.forceBuild || cache.StaticHash != staticHash || !staticExists
	}

	switch {
	case componentStale:
		if err := runBuildPhase(cfg, "component", componentCmd); err != nil {
			return status, err
		}

		status.ComponentBuilt = true
		if err := runCompile(cfg); err != nil {
			return status, err
		}

		if digest, err := fileDigest(wasmPath); err == nil {
			status.WasmDigest = digest
			cache.WasmDigest = digest
		}

		if hash, err := componentInputHash(cfg.dir, m, componentCmd); err == nil {
			cache.ComponentHash = hash
		} else {
			cache.ComponentHash = componentHash
		}

		writeBuildCache(cfg.dir, cache)
	case hasComponentBuild, wasmExists:
		status.ComponentReused = true
	}

	switch {
	case staticRelevant && staticStale:
		if err := runBuildPhase(cfg, "static", staticCmd); err != nil {
			return status, err
		}

		status.StaticBuilt = true

		if hash, err := staticInputHash(cfg.dir, m, staticCmd); err == nil {
			cache.StaticHash = hash
		} else {
			cache.StaticHash = staticHash
		}

		writeBuildCache(cfg.dir, cache)

		if info, err := os.Stat(staticDir); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "warning: static build finished but %s is missing\n", staticDir)
		}
	case staticRelevant:
		status.StaticReused = true
	}

	if status.WasmDigest == "" {
		if digest, err := fileDigest(wasmPath); err == nil {
			status.WasmDigest = digest
		}
	}

	return status, nil
}

// printBuildStatus writes the phase summary to stderr, keeping stdout free for JSON and bundle output.
func printBuildStatus(status buildStatus) {
	if status.BuildSkipped {
		fmt.Fprintln(os.Stderr, "build: skipped")
		return
	}

	switch {
	case status.ComponentBuilt:
		fmt.Fprintln(os.Stderr, "component: built")
	case status.ComponentReused:
		fmt.Fprintln(os.Stderr, "component: reused")
	}

	switch {
	case status.StaticBuilt:
		fmt.Fprintln(os.Stderr, "static: built")
	case status.StaticReused:
		fmt.Fprintln(os.Stderr, "static: reused")
	case status.StaticSkipped:
		fmt.Fprintln(os.Stderr, "static: skipped")
	default:
		fmt.Fprintln(os.Stderr, "static: no build command")
	}
}

// keepRunningComponent reports whether a deploy can skip the wasm payload because the supervisor already runs this digest.
func keepRunningComponent(forceBuild bool, status buildStatus, serverDigest string) bool {
	return !forceBuild && status.WasmDigest != "" && status.WasmDigest == serverDigest
}

// serverDigest returns the currently running component digest, or an empty string when the app does not exist.
func serverDigest(cfg *deployConfig, name string) string {
	req, err := httpRequest(cfg, "GET", "/apps/"+name, nil)
	if err != nil {
		return ""
	}

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}

	var info struct {
		CurrentDigest string `json:"currentDigest"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return ""
	}

	return info.CurrentDigest
}

// httpRequest creates a supervisor API request, adding the bearer token and the base URL from the deploy config.
func httpRequest(cfg *deployConfig, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, baseURL(cfg.host)+path, body)
	if err != nil {
		return nil, err
	}

	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}

	return req, nil
}
