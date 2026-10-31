package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const artifactName = "artifact.json"

// artifactInfo records how a bundle's main.cwasm was produced, so a runtime upgrade can detect a stale artifact.
type artifactInfo struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at,omitempty"`
	// Epoch marks a cwasm compiled with epoch interruption, which the host must enable and per-request timeouts require.
	Epoch      bool   `json:"epoch_interruption,omitempty"`
	WasmSHA256 string `json:"wasm_sha256,omitempty"`
	WasmBytes  int64  `json:"wasm_bytes,omitempty"`
}

// artifactKey names a cached cwasm by the wasm content and the runtime that compiled it.
type artifactKey struct {
	WasmSHA256 string
	Wasmtime   string
	Target     string
	Epoch      bool
}

func (k artifactKey) digest() string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\n%s\n%s\n%t\n", k.WasmSHA256, k.Wasmtime, k.Target, k.Epoch)
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

// wasmtimeVersion reports the configured runtime's version, cached because it only changes with the binary.
func (s *supervisor) wasmtimeVersion() string {
	s.versionOnce.Do(func() {
		out, err := exec.Command(s.cfg.wasmtime, "--version").Output()
		if err != nil {
			log.Printf("wasmtime version: %v", err)
			return
		}
		s.version = strings.TrimSpace(string(out))
	})
	return s.version
}

// artifactMatches reports whether a recorded artifact was built for the current runtime, target and wasm.
func artifactMatches(recorded, current artifactInfo) bool {
	if recorded.Target != current.Target {
		return false
	}
	if current.Epoch && !recorded.Epoch {
		return false
	}
	// A cwasm without the current wasm digest cannot be proven current, so rebuild rather than reuse a stale artifact.
	if current.WasmSHA256 != "" && recorded.WasmSHA256 != current.WasmSHA256 {
		return false
	}
	return recorded.Wasmtime == "" || current.Wasmtime == "" || recorded.Wasmtime == current.Wasmtime
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
	if err != nil || !info.Mode().IsRegular() {
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
	return nil
}

// importCachedArtifact stores an uploaded cwasm under its content key, reusing an existing entry.
func (s *supervisor) importCachedArtifact(src string, key artifactKey) (string, error) {
	dst := s.artifactCachePath(key)
	if _, err := os.Stat(dst); err == nil {
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
	return dst, nil
}

// compileCachedArtifact compiles main.wasm into the cache through a temporary file, never writing the live cwasm in place.
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

	cmd := exec.Command(s.cfg.wasmtimeCompiler, "compile",
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
		return "", err
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
	var files []cacheEntry
	var total int64
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
	if total <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, file := range files {
		if total <= limit {
			break
		}
		if err := os.Remove(file.path); err != nil {
			log.Printf("artifact cache: remove %s: %v", filepath.Base(file.path), err)
			continue
		}
		total -= file.size
	}
}

// ensureArtifact makes sure dir holds a main.cwasm built for this wasm and the configured runtime.
// Bundles keep main.wasm and artifact.json so an upgraded wasmtime can rebuild the artifact instead of failing to load it.
// requireEpoch asks for instrumentation that supports a per-request wasmtime timeout; the returned bool reports whether the artifact has it.
func (s *supervisor) ensureArtifact(dir string, requireEpoch bool) (bool, error) {
	cwasm := filepath.Join(dir, "main.cwasm")
	wasm := filepath.Join(dir, "main.wasm")
	digest, size, err := wasmFileInfo(dir)
	if err != nil {
		if _, statErr := os.Stat(cwasm); statErr == nil {
			if requireEpoch {
				log.Printf("app artifact %s: request_timeout needs epoch interruption, but the bundle has no main.wasm to rebuild from", filepath.Base(dir))
			}
			return false, nil
		}
		return false, fmt.Errorf("%w: no main.wasm or main.cwasm", errNoComponent)
	}

	current := artifactInfo{Wasmtime: s.wasmtimeVersion(), Target: s.cfg.wasmtimeTarget, Epoch: true, WasmSHA256: digest, WasmBytes: size}
	key := artifactKey{WasmSHA256: digest, Wasmtime: current.Wasmtime, Target: current.Target, Epoch: true}
	if path, modTime, ok := s.cachedArtifact(key); ok {
		if err := installArtifact(path, cwasm); err != nil {
			return false, fmt.Errorf("install cached main.cwasm: %w", err)
		}
		current.CompiledAt = modTime
		if err := writeArtifact(dir, current); err != nil {
			log.Printf("app artifact %s: write %s: %v", filepath.Base(dir), artifactName, err)
		}
		s.pruneArtifactCache(path)
		return true, nil
	}

	recorded, recordErr := readArtifact(dir)
	if _, err := os.Stat(cwasm); err == nil && recordErr == nil && artifactMatches(recorded, current) {
		path, err := s.importCachedArtifact(cwasm, key)
		if err != nil {
			return false, fmt.Errorf("cache uploaded main.cwasm: %w", err)
		}
		current.CompiledAt = recorded.CompiledAt
		if err := writeArtifact(dir, current); err != nil {
			log.Printf("app artifact %s: write %s: %v", filepath.Base(dir), artifactName, err)
		}
		s.pruneArtifactCache(path)
		return recorded.Epoch, nil
	}

	log.Printf("app artifact %s: compiling main.wasm for %s (%s)", filepath.Base(dir), current.Wasmtime, current.Target)
	path, err := s.compileCachedArtifact(dir, wasm, key)
	if err != nil {
		// A runtime-only wasmtime cannot compile. Keep a precompiled artifact so upgrades still work when nothing changed.
		if _, statErr := os.Stat(cwasm); statErr == nil {
			if recordErr == nil && recorded.WasmSHA256 != "" && recorded.WasmSHA256 != digest {
				return false, fmt.Errorf("main.cwasm is for wasm %s, not %s: %w", recorded.WasmSHA256, digest, err)
			}
			if requireEpoch && (recordErr != nil || !recorded.Epoch) {
				log.Printf("app artifact %s: request_timeout needs epoch interruption, but rebuilding failed: %v", filepath.Base(dir), err)
			} else {
				log.Printf("app artifact %s: keeping existing main.cwasm: %v", filepath.Base(dir), err)
			}
			if recordErr == nil && artifactMatches(recorded, current) {
				return recorded.Epoch, nil
			}
			return false, nil
		}
		return false, fmt.Errorf("compile main.wasm: %w", err)
	}
	if err := installArtifact(path, cwasm); err != nil {
		return false, fmt.Errorf("install compiled main.cwasm: %w", err)
	}

	current.CompiledAt = time.Now().UTC()
	if err := writeArtifact(dir, current); err != nil {
		log.Printf("app artifact %s: write %s: %v", filepath.Base(dir), artifactName, err)
	}
	s.pruneArtifactCache(path)
	return true, nil
}

// sourceDigest returns the digest a deploy and running app are compared by: the wasm when present, else the legacy cwasm.
func sourceDigest(dir string) string {
	if digest, err := fileDigest(filepath.Join(dir, "main.wasm")); err == nil {
		return digest
	}
	if info, err := readArtifact(dir); err == nil && info.WasmSHA256 != "" {
		return info.WasmSHA256
	}
	if digest, err := fileDigest(filepath.Join(dir, "main.cwasm")); err == nil {
		return digest
	}
	return ""
}
