package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// devTargetProbes are tried in order until the local wasmtime accepts one.
var devTargetProbes = []string{
	"x86_64-unknown-linux-musl",
	"x86_64-unknown-linux-gnu",
	"aarch64-unknown-linux-musl",
	"aarch64-unknown-linux-gnu",
	"x86_64-apple-darwin",
	"aarch64-apple-darwin",
}

// detectTarget asks the local wasmtime which triple it serves, using a tiny precompiled module as a probe.
func detectTarget(wasmtime string) (string, error) {
	dir, err := os.MkdirTemp("", "wt-dev-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "probe.wat")
	if err := os.WriteFile(src, []byte("(module)"), privateFileMode); err != nil {
		return "", err
	}

	artifact := filepath.Join(dir, "probe.cwasm")

	cmd := exec.CommandContext(context.Background(), wasmtime, "compile", "--target", devTargetProbes[0], src, "-o", artifact)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("wasmtime compile probe: %w: %s", err, strings.TrimSpace(string(out)))
	}

	for _, target := range devTargetProbes {
		ctx, cancel := context.WithTimeout(context.Background(), devProbeTimeout)
		out, err := exec.CommandContext(ctx, wasmtime, "serve",
			"--allow-precompiled", "--addr", "127.0.0.1:0", "--target", target, artifact).CombinedOutput()

		cancel()

		if err == nil {
			return target, nil
		}

		if strings.Contains(string(out), "does not match the host") {
			continue
		}

		return target, nil
	}

	return "", fmt.Errorf("no usable target for %s (tried %s); pass --target", wasmtime, strings.Join(devTargetProbes, ", "))
}

// resolveDevTarget picks an explicit wasmtime target or detects one from the local runtime.
func resolveDevTarget(cfg *devConfig) (string, error) {
	if cfg.target != "" {
		return cfg.target, nil
	}

	target, err := detectTarget(cfg.wasmtime)
	if err != nil {
		return "", err
	}

	fmt.Fprintln(os.Stderr, "dev: using target", target)

	return target, nil
}
