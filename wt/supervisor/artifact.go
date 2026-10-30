package main

import (
	"encoding/json/v2"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const artifactName = "artifact.json"

// artifactInfo records how a bundle's main.cwasm was produced, so a runtime upgrade can detect a stale artifact.
type artifactInfo struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at,omitempty"`
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

func writeArtifact(dir string, info artifactInfo) error {
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

// artifactMatches reports whether a recorded artifact was built for the current runtime and target.
func artifactMatches(recorded, current artifactInfo) bool {
	if recorded.Target != current.Target {
		return false
	}
	// A missing version on either side cannot be compared; keep the existing artifact and let wasmtime judge it.
	return recorded.Wasmtime == "" || current.Wasmtime == "" || recorded.Wasmtime == current.Wasmtime
}

// compileArtifact rebuilds main.cwasm from main.wasm, writing through a temporary file so a failed compile keeps the old artifact.
func (s *supervisor) compileArtifact(dir, wasm, cwasm string) error {
	out, err := os.CreateTemp(dir, ".main-*.cwasm")
	if err != nil {
		return err
	}
	temp := out.Name()
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Remove(temp); err != nil {
		return err
	}

	cmd := exec.Command(s.cfg.wasmtimeCompiler, "compile",
		"--target", s.cfg.wasmtimeTarget,
		"-W", "component-model-async=y",
		wasm, "-o", temp)
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("%s compile: %w: %s", filepath.Base(s.cfg.wasmtimeCompiler), err, strings.TrimSpace(string(output)))
	}
	if err := os.Rename(temp, cwasm); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// ensureArtifact makes sure dir holds a main.cwasm built for the configured runtime.
// Bundles keep main.wasm and artifact.json so an upgraded wasmtime can rebuild the artifact instead of failing to load it.
func (s *supervisor) ensureArtifact(dir string) error {
	cwasm := filepath.Join(dir, "main.cwasm")
	wasm := filepath.Join(dir, "main.wasm")
	if _, err := os.Stat(wasm); err != nil {
		if _, statErr := os.Stat(cwasm); statErr == nil {
			return nil
		}
		return fmt.Errorf("%w: no main.wasm or main.cwasm", errNoComponent)
	}

	current := artifactInfo{Wasmtime: s.wasmtimeVersion(), Target: s.cfg.wasmtimeTarget}
	if _, err := os.Stat(cwasm); err == nil {
		if recorded, err := readArtifact(dir); err == nil && artifactMatches(recorded, current) {
			return nil
		}
	}

	log.Printf("app artifact %s: rebuilding main.cwasm for %s (%s)", filepath.Base(dir), current.Wasmtime, current.Target)
	if err := s.compileArtifact(dir, wasm, cwasm); err != nil {
		// A runtime-only wasmtime cannot compile. Keep a precompiled artifact so upgrades still work when nothing changed.
		if _, statErr := os.Stat(cwasm); statErr == nil {
			log.Printf("app artifact %s: keeping existing main.cwasm: %v", filepath.Base(dir), err)
			return nil
		}
		return fmt.Errorf("compile main.wasm: %w", err)
	}

	current.CompiledAt = time.Now().UTC()
	if err := writeArtifact(dir, current); err != nil {
		log.Printf("app artifact %s: write %s: %v", filepath.Base(dir), artifactName, err)
	}
	return nil
}
