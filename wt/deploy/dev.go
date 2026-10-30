package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type devConfig struct {
	dir, wasmtime, target, supervisor, appHost string
	port                                       int
	skipBuild, skipCompile                     bool
}

func resolveDevConfig(args []string) (*devConfig, error) {
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	port := flags.Int("port", 0, "loopback port for the dev supervisor (default 8080)")
	wasmtime := flags.String("wasmtime", env("WT_WASMTIME", "wasmtime"), "wasmtime binary")
	target := flags.String("target", env("WT_WASMTIME_TARGET", ""), "wasmtime compile target (default: match the local wasmtime)")
	supervisor := flags.String("supervisor", env("WT_SUPERVISOR", ""), "supervisor binary (default: PATH, then build from ../../supervisor)")
	appHost := flags.String("app-host", "127.0.0.1", "override the app host so http://127.0.0.1:port/ routes to the app")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build command")
	skipCompile := flags.Bool("skip-compile", false, "reuse an existing main.cwasm")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	resolved := *port
	if resolved == 0 {
		resolved = 8080
		if value := strings.TrimSpace(os.Getenv("WT_DEV_PORT")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 65535 {
				return nil, fmt.Errorf("invalid WT_DEV_PORT %q", value)
			}
			resolved = parsed
		}
	}
	return &devConfig{
		dir:         *dir,
		port:        resolved,
		wasmtime:    *wasmtime,
		target:      *target,
		supervisor:  *supervisor,
		appHost:     *appHost,
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
	}, nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

func dirHasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
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
		return "", fmt.Errorf("no wt-supervisor in PATH; pass --supervisor or set WT_SUPERVISOR")
	}
	out := filepath.Join(workDir, "wt-supervisor")
	fmt.Fprintln(os.Stderr, "dev: building supervisor from", src)
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build supervisor: %w", err)
	}
	return out, nil
}

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
	if err := os.WriteFile(src, []byte("(module)"), 0o644); err != nil {
		return "", err
	}
	artifact := filepath.Join(dir, "probe.cwasm")
	cmd := exec.Command(wasmtime, "compile", "--target", devTargetProbes[0], src, "-o", artifact)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("wasmtime compile probe: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, target := range devTargetProbes {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor did not answer %s within %s", url, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// devEnv overrides the supervisor settings for local development, keeping the rest of the environment.
func devEnv(appsDir, stateDir, addr, wasmtime, target string) []string {
	values := map[string]string{
		"WT_APPS_DIR":        appsDir,
		"WT_STATE_DIR":       stateDir,
		"WT_LISTEN":          addr,
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

func runDev(args []string) error {
	cfg, err := resolveDevConfig(args)
	if err != nil {
		return err
	}
	m, err := readManifest(cfg.dir)
	if err != nil {
		return err
	}
	workDir := filepath.Join(os.TempDir(), "wt-dev", m.Name)
	appsDir := filepath.Join(workDir, "apps")
	stateDir := filepath.Join(workDir, "state")
	for _, dir := range []string{appsDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	supervisor, err := findSupervisor(cfg, workDir)
	if err != nil {
		return err
	}
	target := cfg.target
	if target == "" {
		target, err = detectTarget(cfg.wasmtime)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "dev: using target", target)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.port)
	cmd := exec.Command(supervisor)
	cmd.Env = devEnv(appsDir, stateDir, addr, cfg.wasmtime, target)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()
	if err := waitHealthy("http://"+addr+"/healthz", 20*time.Second); err != nil {
		return err
	}

	dcfg := &deployConfig{
		dir:         cfg.dir,
		host:        "http://" + addr,
		token:       "dev",
		wasmtime:    cfg.wasmtime,
		target:      target,
		appHost:     cfg.appHost,
		skipBuild:   cfg.skipBuild,
		skipCompile: cfg.skipCompile,
	}
	m, data, err := buildBundle(dcfg)
	if err != nil {
		return err
	}
	if err := upload(dcfg, m, data); err != nil {
		return err
	}
	fmt.Printf("dev: %s at http://%s/ (Ctrl-C to stop)\n", m.Name, addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Fprintln(os.Stderr, "\ndev: stopping")
	return nil
}
