package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const artifactName = "artefact.json"

// artifact records how main.cwasm was produced so the supervisor can cache or rebuild it by wasm content.
type artifact struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at"`
	// Epoch records epoch-interruption instrumentation, which per-request wasmtime timeouts require.
	Epoch      bool   `json:"epoch_interruption,omitempty"`
	WasmSHA256 string `json:"wasm_sha256,omitempty"`
	WasmBytes  int64  `json:"wasm_bytes,omitempty"`
	// CPU records the feature policy; compile always passes --target, so it is the baseline set.
	CPU string `json:"cpu,omitempty"`
}

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

func wasmtimeVersion(bin string) (string, error) {
	out, err := exec.CommandContext(context.Background(), bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("wasmtime --version: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// writeArtifact records the wasm digest, size and compiler details so the supervisor can key its cache and detect a stale cwasm.
func writeArtifact(cfg *deployConfig, epoch bool) error {
	wasm := filepath.Join(cfg.dir, "main.wasm")

	info, err := os.Stat(wasm)
	if err != nil {
		return fmt.Errorf("read main.wasm: %w", err)
	}

	digest, err := fileDigest(wasm)
	if err != nil {
		return fmt.Errorf("digest main.wasm: %w", err)
	}

	record := artifact{Target: cfg.target, CPU: "baseline", CompiledAt: time.Now().UTC(), Epoch: epoch, WasmSHA256: digest, WasmBytes: info.Size()}

	version, err := wasmtimeVersion(cfg.wasmtime)
	if err != nil {
		// Without a version the supervisor can still key and rebuild the artifact from main.wasm.
		fmt.Fprintf(os.Stderr, "warning: %v; recording %s without a runtime version\n", err, artifactName)
	} else {
		record.Wasmtime = version
	}

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(cfg.dir, artifactName), append(data, '\n'), privateFileMode)
}

func readArtifactFile(dir string) (artifact, error) {
	var info artifact

	data, err := os.ReadFile(filepath.Join(dir, artifactName))
	if err != nil {
		return info, err
	}

	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}

	return info, nil
}

// runCompile compiles main.wasm through a temporary file, so a failed compile leaves any previous cwasm intact.
// It always passes --target, so the cwasm uses the target's baseline CPU feature set rather than native CPU features.
func runCompile(cfg *deployConfig) error {
	wasm := filepath.Join(cfg.dir, "main.wasm")
	if _, err := os.Stat(wasm); err != nil {
		return fmt.Errorf("read main.wasm: %w (did the build run?)", err)
	}

	cwasm := filepath.Join(cfg.dir, "main.cwasm")
	if cfg.skipCompile {
		digest, err := fileDigest(wasm)
		if err != nil {
			return fmt.Errorf("digest main.wasm: %w", err)
		}

		artifact, artifactErr := readArtifactFile(cfg.dir)
		verified := artifactErr == nil && artifact.WasmSHA256 == digest

		epoch := verified && artifact.Epoch
		if !verified {
			stale := cwasm + ".stale"
			if err := os.Rename(cwasm, stale); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("move unverified main.cwasm aside: %w", err)
			}

			if _, err := os.Stat(stale); err == nil {
				fmt.Fprintf(os.Stderr, "warning: main.cwasm was not verified against main.wasm; moved it to %s\n", filepath.Base(stale))
			}
		}

		if _, err := os.Stat(cwasm); err != nil {
			fmt.Fprintln(os.Stderr, "warning: --skip-compile and no verified main.cwasm; the supervisor will compile main.wasm if it has a compiler")
		}

		return writeArtifact(cfg, epoch)
	}

	temp, err := os.CreateTemp(cfg.dir, ".main-*.cwasm")
	if err != nil {
		return err
	}

	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}

	if err := os.Remove(name); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), cfg.wasmtime, "compile", "--target", cfg.target, "-W", "component-model-async=y", "-W", "epoch-interruption=y", wasm, "-o", name)
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("wasmtime compile: %w", err)
	}

	if err := os.Rename(name, cwasm); err != nil {
		_ = os.Remove(name)
		return err
	}

	return writeArtifact(cfg, true)
}

// buildStatus reports which phases ran, which were reused and the resulting component digest.
type buildStatus struct {
	ComponentBuilt, ComponentReused, StaticBuilt, StaticReused, StaticSkipped, BuildSkipped bool
	WasmDigest                                                                              string
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

	if componentStale && m.Contract == "valibot" {
		if err := generateContract(cfg.dir, webDirName(m), m.Name); err != nil {
			return status, err
		}
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
