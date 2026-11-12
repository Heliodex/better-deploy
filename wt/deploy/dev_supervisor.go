package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}

	return info.Mode()&0o111 != 0
}

// findSupervisor locates the supervisor binary: --supervisor, WT_SUPERVISOR, PATH, then a monorepo sibling build.
func findSupervisor(cfg *devConfig, workDir string) (string, error) {
	if cfg.supervisor != "" {
		if !executable(cfg.supervisor) {
			return "", fmt.Errorf("supervisor %q is not executable", cfg.supervisor)
		}

		return cfg.supervisor, nil
	}

	value := strings.TrimSpace(os.Getenv("WT_SUPERVISOR"))
	if value != "" && !executable(value) {
		return "", fmt.Errorf("supervisor %q is not executable", value)
	}

	if value != "" {
		return value, nil
	}

	if path, err := exec.LookPath("wt-supervisor"); err == nil {
		return path, nil
	}

	src, err := filepath.Abs(filepath.Join(cfg.dir, "..", "..", "supervisor"))
	if err != nil || !dirHasGoMod(src) {
		return "", errors.New("no wt-supervisor in PATH; pass --supervisor or set WT_SUPERVISOR")
	}

	out := filepath.Join(workDir, "wt-supervisor")

	fmt.Fprintln(os.Stderr, "dev: building supervisor from", src)

	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build supervisor: %w", err)
	}

	return out, nil
}

// waitHealthy polls url until it answers or the timeout expires.
func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(request)
		if err == nil {
			resp.Body.Close()
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor did not answer %s within %s", url, timeout)
		}

		time.Sleep(devRetryDelay)
	}
}

// devEnv overrides the supervisor settings for local development, keeping the rest of the environment.
func devEnv(appsDir, stateDir, addr, adminAddr, wasmtime, target string) []string {
	values := map[string]string{
		"WT_APPS_DIR":        appsDir,
		"WT_STATE_DIR":       stateDir,
		"WT_LISTEN":          addr,
		"WT_ADMIN_LISTEN":    adminAddr,
		"WT_APPS_DOMAIN":     "localhost",
		"WT_WASMTIME":        wasmtime,
		"WT_WASMTIME_TARGET": target,
		"WT_TOKEN":           "dev",
		"WT_TLS_CERT":        "",
		"WT_TLS_KEY":         "",
	}

	out := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := values[key]; skip {
			continue
		}

		out = append(out, entry)
	}

	for _, key := range slices.Sorted(maps.Keys(values)) {
		out = append(out, key+"="+values[key])
	}

	return out
}

// prepareDevState creates the apps and state directories the dev supervisor uses.
func prepareDevState(workDir string) (appsDir, stateDir string, err error) {
	appsDir = filepath.Join(workDir, "apps")

	stateDir = filepath.Join(workDir, "state")
	for _, dir := range []string{appsDir, stateDir} {
		if err := os.MkdirAll(dir, privateDirMode); err != nil {
			return "", "", fmt.Errorf("create %s: %w", dir, err)
		}
	}

	return appsDir, stateDir, nil
}

// devAddresses returns the loopback app and admin listener addresses.
func devAddresses(cfg *devConfig) (addr, adminAddr string) {
	return fmt.Sprintf("127.0.0.1:%d", cfg.port), fmt.Sprintf("127.0.0.1:%d", cfg.adminPort)
}

// startDevSupervisor starts the supervisor binary and waits for its health endpoint.
func startDevSupervisor(supervisor string, cfg *devConfig, appsDir, stateDir, addr, adminAddr, target string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(context.Background(), supervisor)
	cmd.Env = devEnv(appsDir, stateDir, addr, adminAddr, cfg.wasmtime, target)
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start supervisor: %w", err)
	}

	if err := waitHealthy("http://"+addr+"/healthz", devProbeTimeout); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()

		return nil, err
	}

	return cmd, nil
}
