package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const artifactName = "artefact.json"

// artifactInfo records how a bundle's main.cwasm was produced, so a runtime upgrade can detect a stale artifact.
type artifactInfo struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at"`
	// Epoch marks a cwasm compiled with epoch interruption, which the host must enable and per-request timeouts require.
	Epoch      bool   `json:"epoch_interruption,omitempty"`
	WasmSHA256 string `json:"wasm_sha256,omitempty"`
	WasmBytes  int64  `json:"wasm_bytes,omitempty"`
	// CPU records the CPU feature policy used for compilation.
	// Both compilers pass --target, so this is the target's baseline feature set.
	CPU string `json:"cpu,omitempty"`
}

// artifactCPUFeatures is the feature policy for a cwasm compiled with --target.
// A native compile would need a real feature fingerprint here instead.
const artifactCPUFeatures = "baseline"

// artifactKey names a cached cwasm by the wasm content and the runtime that compiled it.
type artifactKey struct {
	WasmSHA256 string
	Wasmtime   string
	Target     string
	CPU        string
	Epoch      bool
}

func (k artifactKey) digest() string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\n%s\n%s\n%s\n%t\n", k.WasmSHA256, k.Wasmtime, k.Target, k.CPU, k.Epoch)

	return hex.EncodeToString(hash.Sum(nil))
}

func readArtifact(dir string) (artifactInfo, error) {
	var info artifactInfo

	data, err := os.ReadFile(filepath.Join(dir, artifactName))
	if err != nil {
		return info, err
	}

	if err := json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("%s: %w", artifactName, err)
	}

	return info, nil
}

// wasmFileInfo hashes main.wasm and reports its size.
func wasmFileInfo(dir string) (string, int64, error) {
	path := filepath.Join(dir, "main.wasm")

	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}

	digest, err := fileDigest(path)
	if err != nil {
		return "", 0, err
	}

	return digest, info.Size(), nil
}

func writeArtifact(dir string, info artifactInfo) error {
	if info.WasmSHA256 == "" {
		if digest, size, err := wasmFileInfo(dir); err == nil {
			info.WasmSHA256, info.WasmBytes = digest, size
		}
	}

	data, err := json.Marshal(info)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, artifactName), append(data, '\n'), 0o644)
}

// correctArtifactEpoch records the epoch instrumentation a retained cwasm actually needs after wasmtime rejected the opposite setting.
// Bundles from an older deploy can lack artefact.json or carry a stale epoch flag, and a runtime-only image cannot rebuild them.
func (s *supervisor) correctArtifactEpoch(a *app, epoch bool) {
	a.mu.Lock()
	a.wasmEpoch = epoch
	dir := a.dir
	a.mu.Unlock()

	info, err := readArtifact(dir)
	if err != nil {
		info = artifactInfo{}
	}

	info.Epoch = epoch
	if err := writeArtifact(dir, info); err != nil {
		log.Printf("app artifact %s: record epoch interruption %t: %v", filepath.Base(dir), epoch, err)
	}

	log.Printf("app artifact %s: using epoch interruption %t reported by wasmtime", filepath.Base(dir), epoch)
}

// wasmtimeVersion reports the configured runtime's version, cached because it only changes with the binary.
func (s *supervisor) wasmtimeVersion() string {
	s.versionOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), s.cfg.wasmtime, "--version").Output()
		if err != nil {
			log.Printf("wasmtime version: %v", err)
			return
		}

		s.version = strings.TrimSpace(string(out))
	})

	return s.version
}

// runtimeRelease extracts the release from a wasmtime version, ignoring build metadata such as the commit and build date.
func runtimeRelease(version string) string {
	for field := range strings.FieldsSeq(strings.TrimSpace(version)) {
		candidate := strings.Trim(strings.TrimPrefix(field, "v"), "()")

		if !strings.Contains(candidate, ".") || candidate == "" {
			continue
		}

		release := true

		for _, char := range candidate {
			if (char < '0' || char > '9') && char != '.' {
				release = false
				break
			}
		}

		if release {
			return candidate
		}
	}

	return strings.TrimSpace(version)
}

// artifactMatches reports whether a recorded artifact was built for the current runtime release, target and wasm.
func artifactMatches(recorded, current artifactInfo) bool {
	return recorded.Target == current.Target &&
		recorded.CPU == current.CPU &&
		recorded.Epoch == current.Epoch &&
		recorded.WasmSHA256 == current.WasmSHA256 &&
		runtimeRelease(recorded.Wasmtime) == runtimeRelease(current.Wasmtime)
}

func (s *supervisor) artifactCacheDir() string {
	if s.cfg.cacheDir != "" {
		return s.cfg.cacheDir
	}

	return filepath.Join(s.cfg.stateDir, ".cache")
}

func (s *supervisor) artifactCacheMaxBytes() int64 {
	if s.cfg.cacheMaxBytes > 0 {
		return s.cfg.cacheMaxBytes
	}

	return defaultCacheMaxBytes
}

func (s *supervisor) artifactCachePath(key artifactKey) string {
	return filepath.Join(s.artifactCacheDir(), "cwasm", key.digest()+".cwasm")
}

// cachedArtifact returns a compiled artifact and its modification time when the cache already holds this key.
func (s *supervisor) cachedArtifact(key artifactKey) (string, time.Time, bool) {
	path := s.artifactCachePath(key)

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", time.Time{}, false
	}

	return path, info.ModTime().UTC(), true
}

// installArtifact puts src at dst through a temporary hardlink or copy, so a running process never sees a partial cwasm.
func installArtifact(src, dst string) error {
	temp, err := os.CreateTemp(filepath.Dir(dst), ".main-*.cwasm")
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

	if err := os.Link(src, name); err != nil {
		if err := copyFile(src, name); err != nil {
			_ = os.Remove(name)
			return err
		}
	}

	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return err
	}

	if err := os.Chmod(dst, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}

	return nil
}

// importCachedArtifact stores an uploaded cwasm under its content key, reusing an existing entry.
func (s *supervisor) importCachedArtifact(src string, key artifactKey) (string, error) {
	dst := s.artifactCachePath(key)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Chmod(dst, 0o644); err != nil {
			return "", fmt.Errorf("chmod %s: %w", dst, err)
		}

		return dst, nil
	}

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	temp, err := os.CreateTemp(dir, ".import-*.cwasm")
	if err != nil {
		return "", err
	}

	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}

	if err := os.Remove(name); err != nil {
		return "", err
	}

	if err := copyFile(src, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}

	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return "", err
	}

	if err := os.Chmod(dst, 0o644); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dst, err)
	}

	return dst, nil
}

// compileCachedArtifact compiles main.wasm into the cache through a temporary file, never writing the live cwasm in place.
// The explicit --target selects the target's baseline CPU features, which is what artifactCPUFeatures records.
func (s *supervisor) compileCachedArtifact(dir, wasm string, key artifactKey) (string, error) {
	dst := s.artifactCachePath(key)

	cacheDir := filepath.Dir(dst)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	out, err := os.CreateTemp(cacheDir, ".compile-*.cwasm")
	if err != nil {
		return "", err
	}

	temp := out.Name()
	if err := out.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}

	if err := os.Remove(temp); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(context.Background(), s.cfg.wasmtimeCompiler, "compile",
		"--target", s.cfg.wasmtimeTarget,
		"-W", "component-model-async=y",
		"-W", "epoch-interruption=y",
		wasm, "-o", temp)
	cmd.Dir = dir

	cmd.Env = minimalEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("%s compile: %w: %s", filepath.Base(s.cfg.wasmtimeCompiler), err, strings.TrimSpace(string(output)))
	}

	if err := os.Rename(temp, dst); err != nil {
		_ = os.Remove(temp)

		if _, statErr := os.Stat(dst); statErr == nil {
			if chmodErr := os.Chmod(dst, 0o644); chmodErr != nil {
				return "", fmt.Errorf("chmod %s: %w", dst, chmodErr)
			}

			return dst, nil
		}

		return "", err
	}

	if err := os.Chmod(dst, 0o644); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dst, err)
	}

	return dst, nil
}

// pruneArtifactCache removes the oldest entries until the cache fits its byte cap, never removing the entry just stored.
func (s *supervisor) pruneArtifactCache(keep string) {
	dir := filepath.Join(s.artifactCacheDir(), "cwasm")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type cacheEntry struct {
		path string
		size int64
		mod  time.Time
	}

	var (
		files []cacheEntry
		total int64
	)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cwasm") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		total += info.Size()

		if path == keep {
			continue
		}

		files = append(files, cacheEntry{path: path, size: info.Size(), mod: info.ModTime()})
	}

	limit := s.artifactCacheMaxBytes()

	budget := byteBudget{name: "artifact cache", limit: limit}
	if !budget.exceeded(total) {
		return
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	for _, file := range files {
		if !budget.exceeded(total) {
			break
		}

		if err := os.Remove(file.path); err != nil {
			log.Printf("artifact cache: remove %s: %v", filepath.Base(file.path), err)
			continue
		}

		total -= file.size
	}
}

// artifactForWasm reads the bundle wasm digest and the cache key it maps to.
func (s *supervisor) artifactForWasm(dir, cwasm string, requireEpoch bool) (artifactInfo, artifactKey, bool, error) {
	digest, size, err := wasmFileInfo(dir)
	if err != nil {
		if _, statErr := os.Stat(cwasm); statErr == nil {
			if requireEpoch {
				log.Printf("app artifact %s: request_timeout needs epoch interruption, but the bundle has no main.wasm to rebuild from", filepath.Base(dir))
			}

			return artifactInfo{}, artifactKey{}, false, nil
		}

		return artifactInfo{}, artifactKey{}, false, fmt.Errorf("%w: no main.wasm or main.cwasm", errNoComponent)
	}

	current := artifactInfo{Wasmtime: s.wasmtimeVersion(), Target: s.cfg.wasmtimeTarget, CPU: artifactCPUFeatures, Epoch: true, WasmSHA256: digest, WasmBytes: size}
	key := artifactKey{WasmSHA256: digest, Wasmtime: runtimeRelease(current.Wasmtime), Target: current.Target, CPU: current.CPU, Epoch: true}

	return current, key, true, nil
}

// finishArtifact records the installed artifact and prunes the cache to its limits.
func (s *supervisor) finishArtifact(dir, path string, current artifactInfo, compiledAt time.Time, epoch bool) (bool, error) {
	current.CompiledAt = compiledAt
	if err := writeArtifact(dir, current); err != nil {
		log.Printf("app artifact %s: write %s: %v", filepath.Base(dir), artifactName, err)
	}

	s.pruneArtifactCache(path)

	return epoch, nil
}

// keepExistingArtifact handles a failed compile when the bundle already has a usable main.cwasm.
func (s *supervisor) keepExistingArtifact(dir, cwasm string, recorded artifactInfo, recordErr error, current artifactInfo, requireEpoch bool, compileErr error) (bool, error) {
	if _, statErr := os.Stat(cwasm); statErr != nil {
		return false, fmt.Errorf("compile main.wasm: %w", compileErr)
	}
	// A runtime-only wasmtime cannot compile. Keep a precompiled artifact so upgrades still work when nothing changed.
	if recordErr == nil && recorded.WasmSHA256 != "" && recorded.WasmSHA256 != current.WasmSHA256 {
		return false, fmt.Errorf("main.cwasm is for wasm %s, not %s: %w", recorded.WasmSHA256, current.WasmSHA256, compileErr)
	}

	if requireEpoch && (recordErr != nil || !recorded.Epoch) {
		log.Printf("app artifact %s: request_timeout needs epoch interruption, but rebuilding failed: %v", filepath.Base(dir), compileErr)
	} else {
		log.Printf("app artifact %s: keeping existing main.cwasm: %v", filepath.Base(dir), compileErr)
	}

	if recordErr == nil && artifactMatches(recorded, current) {
		return recorded.Epoch, nil
	}
	// The cwasm was kept, so launch with the epoch flag it was compiled with even when the cache key differs only by a runtime version suffix.
	if recordErr == nil {
		return recorded.Epoch, nil
	}

	return false, nil
}

// ensureArtifact makes sure dir holds a main.cwasm built for this wasm and the configured runtime.
// Bundles keep main.wasm and artefact.json so an upgraded wasmtime can rebuild the artifact instead of failing to load it.
// requireEpoch asks for instrumentation that supports a per-request wasmtime timeout; the returned bool reports whether the artifact has it.
func (s *supervisor) ensureArtifact(dir string, requireEpoch bool) (bool, error) {
	// The cache is shared by every app, so a per-app deploy lock is not enough to protect lookups, installs and pruning.
	s.artifactCacheMu.Lock()
	defer s.artifactCacheMu.Unlock()

	cwasm := filepath.Join(dir, "main.cwasm")
	wasm := filepath.Join(dir, "main.wasm")

	current, key, ok, err := s.artifactForWasm(dir, cwasm, requireEpoch)
	if err != nil {
		return false, err
	}

	if !ok {
		return false, nil
	}

	if path, modTime, ok := s.cachedArtifact(key); ok {
		if err := installArtifact(path, cwasm); err != nil {
			return false, fmt.Errorf("install cached main.cwasm: %w", err)
		}

		return s.finishArtifact(dir, path, current, modTime, true)
	}

	recorded, recordErr := readArtifact(dir)
	if _, err := os.Stat(cwasm); err == nil && recordErr == nil && artifactMatches(recorded, current) {
		path, err := s.importCachedArtifact(cwasm, key)
		if err != nil {
			return false, fmt.Errorf("cache uploaded main.cwasm: %w", err)
		}

		return s.finishArtifact(dir, path, current, recorded.CompiledAt, recorded.Epoch)
	}

	if !s.canCompile() {
		if _, statErr := os.Stat(cwasm); statErr != nil {
			return false, errors.New("no compiler available and no main.cwasm to run")
		}

		if recordErr == nil && recorded.WasmSHA256 != "" && recorded.WasmSHA256 != current.WasmSHA256 {
			return false, fmt.Errorf("main.cwasm is for wasm %s, not %s, and no compiler is available", recorded.WasmSHA256, current.WasmSHA256)
		}

		if requireEpoch && (recordErr != nil || !recorded.Epoch) {
			log.Printf("app artifact %s: request_timeout needs epoch interruption, but no compiler is available", filepath.Base(dir))
		} else {
			log.Printf("app artifact %s: no compiler available; keeping existing main.cwasm", filepath.Base(dir))
		}

		if recordErr == nil {
			return recorded.Epoch, nil
		}

		return false, nil
	}

	log.Printf("app artifact %s: compiling main.wasm for %s (%s)", filepath.Base(dir), current.Wasmtime, current.Target)

	path, err := s.compileCachedArtifact(dir, wasm, key)
	if err != nil {
		return s.keepExistingArtifact(dir, cwasm, recorded, recordErr, current, requireEpoch, err)
	}

	if err := installArtifact(path, cwasm); err != nil {
		return false, fmt.Errorf("install compiled main.cwasm: %w", err)
	}

	return s.finishArtifact(dir, path, current, time.Now().UTC(), true)
}

// sourceDigest returns the digest a deploy and running app are compared by.
func sourceDigest(dir string) string {
	digest, _ := fileDigest(filepath.Join(dir, "main.wasm"))
	return digest
}
