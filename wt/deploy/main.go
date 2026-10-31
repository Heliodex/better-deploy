// Command wt-deploy builds a WASI HTTP component, precompiles it with wasmtime, and uploads it to a wt-supervisor.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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
	Name           string            `json:"name"`
	Host           string            `json:"host"`
	Health         string            `json:"health"`
	Env            map[string]string `json:"env"`
	Build          string            `json:"build"`
	MemoryMB       int               `json:"memory_mb,omitempty"`
	DiskMB         int               `json:"disk_mb,omitempty"`
	MaxConcurrent  int               `json:"max_concurrency,omitempty"`
	RequestTimeout string            `json:"request_timeout,omitempty"`
	Network        string            `json:"network,omitempty"`
	Schedule       []schedule        `json:"schedule,omitempty"`
}

// schedule is carried through the bundle so the supervisor can call the app on a timer.
type schedule struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	Every  string `json:"every"`
}

const artifactName = "artifact.json"

// artifact records how main.cwasm was produced so the supervisor can cache or rebuild it by wasm content.
type artifact struct {
	Wasmtime   string    `json:"wasmtime,omitempty"`
	Target     string    `json:"target,omitempty"`
	CompiledAt time.Time `json:"compiled_at,omitempty"`
	// Epoch records epoch-interruption instrumentation, which per-request wasmtime timeouts require.
	Epoch      bool   `json:"epoch_interruption,omitempty"`
	WasmSHA256 string `json:"wasm_sha256,omitempty"`
	WasmBytes  int64  `json:"wasm_bytes,omitempty"`
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
	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")
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
	record := artifact{Target: cfg.target, CompiledAt: time.Now().UTC(), Epoch: epoch, WasmSHA256: digest, WasmBytes: info.Size()}
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
	return os.WriteFile(filepath.Join(cfg.dir, artifactName), append(data, '\n'), 0o644)
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
func runCompile(cfg *deployConfig) error {
	wasm := filepath.Join(cfg.dir, "main.wasm")
	if _, err := os.Stat(wasm); err != nil {
		return fmt.Errorf("read main.wasm: %w (did the build run?)", err)
	}
	cwasm := filepath.Join(cfg.dir, "main.cwasm")
	if cfg.skipCompile {
		epoch := false
		if info, err := readArtifactFile(cfg.dir); err == nil {
			epoch = info.Epoch
			if digest, digestErr := fileDigest(wasm); digestErr == nil && info.WasmSHA256 != "" && info.WasmSHA256 != digest {
				stale := cwasm + ".stale"
				if err := os.Rename(cwasm, stale); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("move stale main.cwasm aside: %w", err)
				}
				fmt.Fprintf(os.Stderr, "warning: main.cwasm was built for wasm %s; moved it to %s\n", info.WasmSHA256, filepath.Base(stale))
				epoch = false
			}
		}
		if _, err := os.Stat(cwasm); err != nil {
			fmt.Fprintln(os.Stderr, "warning: --skip-compile and no main.cwasm; the supervisor will compile main.wasm if it has a compiler")
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
	cmd := exec.Command(cfg.wasmtime, "compile", "--target", cfg.target, "-W", "component-model-async=y", "-W", "epoch-interruption=y", wasm, "-o", name)
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

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// writeTarPath adds a file from disk to the bundle with its relative name.
func writeTarPath(tw *tar.Writer, name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.CopyN(tw, file, info.Size())
	return err
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
	wasm, err := os.ReadFile(filepath.Join(dir, "main.wasm"))
	if err != nil {
		return nil, fmt.Errorf("read main.wasm: %w (did the build run?)", err)
	}
	if err := writeTarFile(tw, "main.wasm", wasm); err != nil {
		return nil, err
	}
	// main.cwasm is only a precompiled hint; the supervisor can compile from main.wasm or reuse its cache.
	for _, name := range []string{"main.cwasm", artifactName} {
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
	// Static files are served by the supervisor, so a frontend change does not need to be inside the component.
	static := filepath.Join(dir, "static")
	if info, err := os.Stat(static); err == nil && info.IsDir() {
		err := filepath.WalkDir(static, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			return writeTarPath(tw, filepath.ToSlash(rel), path)
		})
		if err != nil {
			return nil, fmt.Errorf("add static files: %w", err)
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
	if digest := resp.Header.Get("X-WT-Wasm-Digest"); digest != "" {
		fmt.Printf("digest: %s deploy mode: %s\n", digest, resp.Header.Get("X-WT-Deploy-Mode"))
	}
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
	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")
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
		fmt.Println("       wt-deploy dev [--dir DIR] [--port 8080] [--wasmtime PATH] [--watch=false]")
		fmt.Println("       wt-deploy new --dir wt/example/NAME")
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}
