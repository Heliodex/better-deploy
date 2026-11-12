package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runBuildPhase runs one named app.json build phase with the app directory as its working directory.
func runBuildPhase(cfg *deployConfig, phase, command string) error {
	if cfg.skipBuild {
		return nil
	}

	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("app.json has no %s build command; set one or pass --skip-build", phase)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)
	cmd.Dir = cfg.dir
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s build: %w", phase, err)
	}

	return nil
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

// generateStaleContract regenerates the Valibot contract only when the component inputs changed.
func generateStaleContract(cfg *deployConfig, m manifest, stale bool) error {
	if !stale || m.Contract != "valibot" {
		return nil
	}

	return generateContract(cfg.dir, webDirName(m), m.Name)
}

// phaseInputs is one build phase's command and the input hash and file count measured for its cache.
type phaseInputs struct {
	cmd   string
	hash  string
	files int
}

// measurePhase hashes a phase's inputs when it has a command, and returns an empty measurement otherwise.
func measurePhase(dir string, m manifest, cmd string, hashFn inputHashFunc) (phaseInputs, error) {
	in := phaseInputs{cmd: cmd}
	if cmd == "" {
		return in, nil
	}

	hash, files, err := hashFn(dir, m, cmd)
	if err != nil {
		return in, err
	}

	in.hash, in.files = hash, files

	return in, nil
}

// remeasure hashes the inputs again after a phase ran, because the build may have written files the hash covers.
// If that fails, the measurement from before the build stands.
func (in phaseInputs) remeasure(dir string, m manifest, hashFn inputHashFunc) phaseInputs {
	hash, files, err := hashFn(dir, m, in.cmd)
	if err != nil {
		return in
	}

	return phaseInputs{cmd: in.cmd, hash: hash, files: files}
}

// inputHashFunc hashes one phase's inputs, such as componentInputHash or staticInputHash.
type inputHashFunc func(dir string, m manifest, command string) (string, int, error)

// buildComponent runs the component command, compiles the result and records the new digest and input hash.
func buildComponent(cfg *deployConfig, m manifest, cache *buildCache, status *buildStatus, in phaseInputs) error {
	if err := runBuildPhase(cfg, "component", in.cmd); err != nil {
		return err
	}

	status.ComponentBuilt = true

	if err := runCompile(cfg); err != nil {
		return err
	}

	if digest, err := fileDigest(filepath.Join(cfg.dir, "main.wasm")); err == nil {
		status.WasmDigest = digest
		cache.WasmDigest = digest
	}

	in = in.remeasure(cfg.dir, m, componentInputHash)
	cache.ComponentHash = in.hash
	status.ComponentHash, status.ComponentFiles = in.hash, in.files

	writeBuildCache(cfg.dir, *cache)

	return nil
}

// runComponentPhase builds the component when its inputs are stale. Otherwise it keeps the compiled component when its metadata still matches, and recompiles it when not.
func runComponentPhase(cfg *deployConfig, m manifest, cache *buildCache, status *buildStatus, in phaseInputs, stale bool) error {
	if stale {
		return buildComponent(cfg, m, cache, status, in)
	}

	status.ComponentHash, status.ComponentFiles = in.hash, in.files
	if artifactUsable(cfg) {
		status.ComponentReused = true

		return nil
	}

	if err := runCompile(cfg); err != nil {
		return err
	}

	status.ComponentRecompiled = true

	return nil
}

// buildStatic runs the static command, then records its new input hash and warns if the output directory is missing.
func buildStatic(cfg *deployConfig, m manifest, cache *buildCache, status *buildStatus, in phaseInputs) error {
	if err := runBuildPhase(cfg, "static", in.cmd); err != nil {
		return err
	}

	status.StaticBuilt = true

	in = in.remeasure(cfg.dir, m, staticInputHash)
	cache.StaticHash = in.hash
	status.StaticHash, status.StaticFiles = in.hash, in.files

	writeBuildCache(cfg.dir, *cache)

	staticDir := filepath.Join(cfg.dir, "static")
	if info, err := os.Stat(staticDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "warning: static build finished but %s is missing\n", staticDir)
	}

	return nil
}

// runStaticPhase builds the static output when stale and otherwise marks it reused. A phase without a command does nothing.
func runStaticPhase(cfg *deployConfig, m manifest, cache *buildCache, status *buildStatus, in phaseInputs, stale bool) error {
	if in.cmd == "" {
		return nil
	}

	if !stale {
		status.StaticReused = true

		return nil
	}

	return buildStatic(cfg, m, cache, status, in)
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

	wasmPath := filepath.Join(cfg.dir, "main.wasm")

	if cfg.skipBuild {
		status.BuildSkipped = true
		if digest, err := fileDigest(wasmPath); err == nil {
			status.WasmDigest = digest
		}

		return status, nil
	}

	wasmInfo, wasmErr := os.Stat(wasmPath)
	wasmExists := wasmErr == nil && wasmInfo.Mode().IsRegular()

	hasComponentBuild := componentCmd != ""
	if !hasComponentBuild && !wasmExists {
		return status, errors.New("app.json has no build_component and main.wasm is missing")
	}

	if !hasComponentBuild && cfg.forceBuild {
		return status, errors.New("cannot force-build without a component build command")
	}

	staticInfo, staticErr := os.Stat(filepath.Join(cfg.dir, "static"))
	staticExists := staticErr == nil && staticInfo.IsDir()

	cache, _ := readBuildCache(cfg.dir)

	component, err := measurePhase(cfg.dir, m, componentCmd, componentInputHash)
	if err != nil {
		return status, err
	}

	static, err := measurePhase(cfg.dir, m, staticCmd, staticInputHash)
	if err != nil {
		return status, err
	}

	componentStale := hasComponentBuild && (cfg.forceBuild || cache.ComponentHash != component.hash || !wasmExists)
	staticStale := static.cmd != "" && (cfg.forceBuild || cache.StaticHash != static.hash || !staticExists)

	if err := generateStaleContract(cfg, m, componentStale); err != nil {
		return status, err
	}

	// The registry outputs follow the endpoint registry, not the component hash: a registry-only change must still regenerate.
	webDir := filepath.Join(cfg.dir, webDirName(m))
	if err := generateRegistryOutputs(cfg.dir, webDir); err != nil {
		return status, err
	}

	if err := runComponentPhase(cfg, m, &cache, &status, component, componentStale); err != nil {
		return status, err
	}

	if err := runStaticPhase(cfg, m, &cache, &status, static, staticStale); err != nil {
		return status, err
	}

	if status.WasmDigest == "" {
		if digest, err := fileDigest(wasmPath); err == nil {
			status.WasmDigest = digest
		}
	}

	return status, nil
}

// httpRequest creates a supervisor API request, adding the bearer token and the base URL from the deploy config.
func httpRequest(cfg *deployConfig, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, baseURL(cfg.host)+path, body)
	if err != nil {
		return nil, err
	}

	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}

	return req, nil
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

	data, err := io.ReadAll(io.LimitReader(resp.Body, smallResponseLimit))
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
