// Command wt-deploy builds a WASI HTTP component, precompiles it with
// wasmtime, and uploads it to a wt-supervisor.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
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
}

type deployConfig struct {
	dir         string
	app         string
	host        string
	token       string
	wasmtime    string
	skipBuild   bool
	skipCompile bool
	appHost     string
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func resolveConfig(args []string) (*deployConfig, error) {
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	app := flags.String("app", "", "app name (default: app.json name or directory name)")
	host := flags.String("host", env("WT_DEPLOY_HOST", ""), "supervisor URL, e.g. http://127.0.0.1:8080")
	token := flags.String("token", env("WT_TOKEN", ""), "deploy bearer token")
	wasmtime := flags.String("wasmtime", env("WT_WASMTIME", "wasmtime"), "wasmtime binary")
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
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
		appHost:     *appHost,
	}, nil
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
	cmd := exec.Command(cfg.wasmtime, "compile", "-W", "component-model-async=y", wasm, "-o", cwasm)
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

	appJSON, err := json.MarshalIndent(m, "", "  ")
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

func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
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
	case "help", "-h", "--help":
		fmt.Println("usage: wt-deploy deploy [--dir DIR] [--host URL] [--token TOKEN] [--wasmtime PATH] [--skip-build] [--skip-compile]")
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}
