package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const artifactName = "artifact.json"

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
