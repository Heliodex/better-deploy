// Command wt-deploy builds a WASI HTTP component, precompiles it with wasmtime, and uploads it to a wt-supervisor.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	Name     string            `json:"name"`
	Host     string            `json:"host"`
	Health   string            `json:"health"`
	Env      map[string]string `json:"env"`
	Build    string            `json:"build"`
	MemoryMB int               `json:"memory_mb,omitempty"`
	DiskMB   int               `json:"disk_mb,omitempty"`
	Schedule []schedule        `json:"schedule,omitempty"`
}

// schedule is carried through the bundle so the supervisor can call the app on a timer.
type schedule struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	Every  string `json:"every"`
}

const artifactName = "artifact.json"

// artifact records how main.cwasm was produced so the supervisor can rebuild it after a wasmtime upgrade.
type artifact struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at,omitempty"`
}

const (
	minScheduleInterval = time.Second
	maxScheduleInterval = 24 * time.Hour
)

type deployConfig struct {
	dir, app, host, token, wasmtime, target, appHost, out string
	skipBuild, skipCompile                                bool
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
	if m.MemoryMB < 0 || m.DiskMB < 0 {
		return m, fmt.Errorf("memory_mb and disk_mb must be 0 or more")
	}
	for i, entry := range m.Schedule {
		if !strings.HasPrefix(entry.Path, "/") {
			return m, fmt.Errorf("schedule[%d]: path must start with /", i)
		}
		every, err := time.ParseDuration(entry.Every)
		if err != nil || every < minScheduleInterval || every > maxScheduleInterval {
			return m, fmt.Errorf("schedule[%d]: every %q must be a duration between %s and %s", i, entry.Every, minScheduleInterval, maxScheduleInterval)
		}
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

func wasmtimeVersion(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("wasmtime --version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// writeArtifact records the compiler version and target beside the bundle so the supervisor can detect a stale cwasm.
func writeArtifact(cfg *deployConfig) error {
	path := filepath.Join(cfg.dir, artifactName)
	if cfg.skipCompile {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	version, err := wasmtimeVersion(cfg.wasmtime)
	if err != nil {
		// Without a version the supervisor can still rebuild main.wasm on a runtime that has a compiler.
		fmt.Fprintf(os.Stderr, "warning: %v; not writing %s\n", err, artifactName)
		return nil
	}
	data, err := json.Marshal(artifact{Wasmtime: version, Target: cfg.target, CompiledAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
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
		return writeArtifact(cfg)
	}
	cwasm := filepath.Join(cfg.dir, "main.cwasm")
	cmd := exec.Command(cfg.wasmtime, "compile", "--target", cfg.target, "-W", "component-model-async=y", wasm, "-o", cwasm)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wasmtime compile: %w", err)
	}
	return writeArtifact(cfg)
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
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
	if err := writeTarFile(tw, "app.json", append(appJSON, '\n')); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "main.cwasm"))
	if err != nil {
		return nil, fmt.Errorf("read main.cwasm: %w (did the compile run?)", err)
	}
	if err := writeTarFile(tw, "main.cwasm", data); err != nil {
		return nil, err
	}
	// main.wasm lets a supervisor with a compiler rebuild the artifact after a runtime upgrade.
	for _, name := range []string{"main.wasm", artifactName} {
		extra, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if err := writeTarFile(tw, name, extra); err != nil {
			return nil, err
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

// buildBundle runs the app build, precompiles the component and packs the upload artifact.
func buildBundle(cfg *deployConfig) (manifest, []byte, error) {
	m, err := readManifest(cfg.dir)
	if err != nil {
		return m, nil, err
	}
	if cfg.app != "" {
		if !validName(cfg.app) {
			return m, nil, fmt.Errorf("invalid app name %q", cfg.app)
		}
		m.Name = cfg.app
	}
	if cfg.appHost != "" {
		m.Host = cfg.appHost
	}
	if err := runBuild(cfg, m); err != nil {
		return m, nil, err
	}
	if err := runCompile(cfg); err != nil {
		return m, nil, err
	}
	data, err := bundle(cfg.dir, m)
	if err != nil {
		return m, nil, err
	}
	return m, data, nil
}

func deploy(args []string) error {
	cfg, err := resolveConfig(args)
	if err != nil {
		return err
	}
	m, data, err := buildBundle(cfg)
	if err != nil {
		return err
	}
	return upload(cfg, m, data)
}

func resolveBundleConfig(args []string) (*deployConfig, error) {
	flags := flag.NewFlagSet("bundle", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	wasmtime := flags.String("wasmtime", env("WT_WASMTIME", "wasmtime"), "wasmtime binary")
	target := flags.String("target", env("WT_WASMTIME_TARGET", "x86_64-unknown-linux-musl"), "wasmtime compile target")
	out := flags.String("out", "", "output file (default <name>.tar.gz)")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build command")
	skipCompile := flags.Bool("skip-compile", false, "reuse an existing main.cwasm")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return &deployConfig{
		dir:         *dir,
		wasmtime:    *wasmtime,
		target:      *target,
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
		out:         *out,
	}, nil
}

// runBundle builds the deploy artifact into a file instead of uploading it, for CI or manual upload.
func runBundle(args []string) error {
	cfg, err := resolveBundleConfig(args)
	if err != nil {
		return err
	}
	m, data, err := buildBundle(cfg)
	if err != nil {
		return err
	}
	out := cfg.out
	if out == "" {
		out = m.Name + ".tar.gz"
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	fmt.Println("wrote", out)
	return nil
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
	case "bundle":
		err = runBundle(args)
	case "dev":
		err = runDev(args)
	case "new":
		err = runNew(args)
	case "help", "-h", "--help":
		fmt.Println("usage: wt-deploy deploy [--dir DIR] [--host URL] [--token TOKEN] [--wasmtime PATH] [--skip-build] [--skip-compile]")
		fmt.Println("       wt-deploy bundle --dir DIR --out app.tar.gz [--wasmtime PATH] [--skip-build] [--skip-compile]")
		fmt.Println("       wt-deploy dev [--dir DIR] [--port 8080] [--wasmtime PATH]")
		fmt.Println("       wt-deploy new --dir wt/example/NAME")
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}
