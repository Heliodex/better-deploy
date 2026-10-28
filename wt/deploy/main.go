// Command wt-deploy builds a WASI HTTP component, precompiles it with wasmtime, and uploads it to a wt-supervisor.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type manifest struct {
	Name     string            `json:"name"`
	Host     string            `json:"host"`
	Health   string            `json:"health"`
	Env      map[string]string `json:"env"`
	Build    string            `json:"build"`
	MemoryMB int               `json:"memory_mb,omitempty"`
}

type deployConfig struct {
	dir, app, host, token, wasmtime, target, appHost string
	skipBuild, skipCompile                           bool
}

type devConfig struct {
	dir, wasmtime, target, supervisor, appHost string
	port                                       int
	skipBuild, skipCompile                     bool
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

func readManifest(dir string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		return m, fmt.Errorf("read app.json: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("app.json: %w", err)
	}
	if m.Name == "" {
		m.Name = filepath.Base(filepath.Clean(dir))
	}
	if !validName(m.Name) {
		return m, fmt.Errorf("invalid app name %q", m.Name)
	}
	return m, nil
}

func resolveConfig(args []string) (*deployConfig, error) {
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	app := flags.String("app", "", "app name (default: app.json name or directory name)")
	host := flags.String("host", env("WT_DEPLOY_HOST", ""), "supervisor URL, e.g. http://127.0.0.1:8080")
	token := flags.String("token", env("WT_TOKEN", ""), "deploy bearer token")
	wasmtime := flags.String("wasmtime", env("WT_WASMTIME", "wasmtime"), "wasmtime binary")
	target := flags.String("target", env("WT_WASMTIME_TARGET", "x86_64-unknown-linux-musl"), "wasmtime compile target")
	appHost := flags.String("app-host", "", "override the app host in the uploaded app.json")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build command")
	skipCompile := flags.Bool("skip-compile", false, "reuse an existing main.cwasm")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *host == "" {
		return nil, fmt.Errorf("set --host or WT_DEPLOY_HOST")
	}
	return &deployConfig{
		dir:         *dir,
		app:         *app,
		host:        *host,
		token:       *token,
		wasmtime:    *wasmtime,
		target:      *target,
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
		appHost:     *appHost,
	}, nil
}

func runBuild(cfg *deployConfig, m manifest) error {
	if cfg.skipBuild {
		return nil
	}
	if strings.TrimSpace(m.Build) == "" {
		return fmt.Errorf("app.json has no build command; set one or pass --skip-build")
	}
	cmd := exec.Command("sh", "-c", m.Build)
	cmd.Dir = cfg.dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	return nil
}

func runCompile(cfg *deployConfig) error {
	wasm := filepath.Join(cfg.dir, "main.wasm")
	if _, err := os.Stat(wasm); err != nil {
		return fmt.Errorf("read main.wasm: %w (did the build run?)", err)
	}
	if cfg.skipCompile {
		if _, err := os.Stat(filepath.Join(cfg.dir, "main.cwasm")); err != nil {
			return fmt.Errorf("--skip-compile but no main.cwasm: %w", err)
		}
		return nil
	}
	cwasm := filepath.Join(cfg.dir, "main.cwasm")
	cmd := exec.Command(cfg.wasmtime, "compile", "--target", cfg.target, "-W", "component-model-async=y", wasm, "-o", cwasm)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wasmtime compile: %w", err)
	}
	return nil
}

func bundle(dir string, m manifest) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	appJSON, err := json.Marshal(m, jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	entries := []struct {
		name string
		data []byte
	}{
		{"app.json", append(appJSON, '\n')},
	}
	for _, name := range []string{"main.wasm", "main.cwasm"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) && name == "main.cwasm" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		entries = append(entries, struct {
			name string
			data []byte
		}{name, data})
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.data))}); err != nil {
			return nil, fmt.Errorf("write %s: %w", entry.name, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			return nil, fmt.Errorf("write %s: %w", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

func upload(cfg *deployConfig, m manifest, bundle []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("artifact", "app.tar.gz")
	if err != nil {
		return err
	}
	if _, err := part.Write(bundle); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}

	url := baseURL(cfg.host) + "/apps/" + m.Name
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post bundle: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deploy failed with status %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	fmt.Println(strings.TrimSpace(string(respBody)))
	return nil
}

func deploy(args []string) error {
	cfg, err := resolveConfig(args)
	if err != nil {
		return err
	}
	m, err := readManifest(cfg.dir)
	if err != nil {
		return err
	}
	if cfg.app != "" {
		if !validName(cfg.app) {
			return fmt.Errorf("invalid app name %q", cfg.app)
		}
		m.Name = cfg.app
	}
	if cfg.appHost != "" {
		m.Host = cfg.appHost
	}
	if err := runBuild(cfg, m); err != nil {
		return err
	}
	if err := runCompile(cfg); err != nil {
		return err
	}
	bundle, err := bundle(cfg.dir, m)
	if err != nil {
		return err
	}
	return upload(cfg, m, bundle)
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
	if dcfg.appHost != "" {
		m.Host = dcfg.appHost
	}
	if err := runBuild(dcfg, m); err != nil {
		return err
	}
	if err := runCompile(dcfg); err != nil {
		return err
	}
	data, err := bundle(cfg.dir, m)
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
	for _, key := range []string{
		"WT_APPS_DIR", "WT_STATE_DIR", "WT_LISTEN", "WT_WASMTIME",
		"WT_WASMTIME_TARGET", "WT_TOKEN", "WT_TLS_CERT", "WT_TLS_KEY",
	} {
		out = append(out, key+"="+values[key])
	}
	return out
}

func main() {
	command := "deploy"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			command = "help"
			break
		}
	}

	var err error
	switch command {
	case "deploy":
		err = deploy(args)
	case "dev":
		err = runDev(args)
	case "help", "-h", "--help":
		fmt.Println("usage: wt-deploy deploy [--dir DIR] [--host URL] [--token TOKEN] [--wasmtime PATH] [--skip-build] [--skip-compile]")
		fmt.Println("       wt-deploy dev [--dir DIR] [--port 8080] [--wasmtime PATH]")
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}
