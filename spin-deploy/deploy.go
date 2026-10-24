package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
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

type deployConfig struct {
	dir       string
	app       string
	host      string
	token     string
	skipBuild bool
}

func defaultAppName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "spin.toml"))
	if err != nil {
		return "", fmt.Errorf("read spin.toml: %w", err)
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "application" || !strings.HasPrefix(line, "name") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "name"))
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(rest, "=")), `"'`)
		if name == "" {
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("no application name found in spin.toml")
}

func resolveDeployConfig(args []string) (*deployConfig, error) {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dir := fs.String("dir", ".", "application directory containing spin.toml")
	app := fs.String("app", os.Getenv("APP"), "app name on the supervisor (default: spin.toml application name)")
	host := fs.String("deploy-host", os.Getenv("DEPLOY_HOST"), "supervisor host, e.g. deploy.apps.example.com")
	token := fs.String("token", os.Getenv("SUPERVISOR_TOKEN"), "bearer token for the deploy API")
	skipBuild := fs.Bool("skip-build", false, "skip spin build and upload the current build output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg := &deployConfig{dir: *dir, app: *app, host: *host, token: *token, skipBuild: *skipBuild}
	if cfg.dir == "" {
		return nil, fmt.Errorf("dir must not be empty")
	}
	if cfg.host == "" {
		return nil, fmt.Errorf("set --deploy-host or DEPLOY_HOST")
	}
	if cfg.token == "" {
		return nil, fmt.Errorf("set --token or SUPERVISOR_TOKEN")
	}
	if cfg.app == "" {
		name, err := defaultAppName(cfg.dir)
		if err != nil {
			return nil, err
		}
		cfg.app = name
	}
	return cfg, nil
}

func buildApp(cfg *deployConfig) error {
	if cfg.skipBuild {
		return nil
	}
	cmd := exec.Command("spin", "build")
	cmd.Dir = cfg.dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("spin build: %w", err)
	}
	return nil
}

func bundleApp(dir string) ([]byte, error) {
	if _, err := os.Stat(filepath.Join(dir, "spin.toml")); err != nil {
		return nil, fmt.Errorf("read spin.toml: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.wasm")); err != nil {
		return nil, fmt.Errorf("read main.wasm: %w (run spin build first)", err)
	}
	names := []string{"spin.toml", "main.wasm"}
	if _, err := os.Stat(filepath.Join(dir, "app.json")); err == nil {
		names = append(names, "app.json")
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat app.json: %w", err)
	} else {
		fmt.Fprintln(os.Stderr, "warning: no app.json; the server will fall back to defaults")
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

func uploadBundle(cfg *deployConfig, bundle []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("artifact", "app.tgz")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(bundle); err != nil {
		return fmt.Errorf("write form file: %w", err)
	}
	if err := form.Close(); err != nil {
		return fmt.Errorf("close form: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL(cfg.host)+"/apps/"+cfg.app, &body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", form.FormDataContentType())

	client := &http.Client{Timeout: 120 * time.Second}
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
	fmt.Print(string(respBody))
	return nil
}

func runDeploy(args []string) error {
	cfg, err := resolveDeployConfig(args)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if err := buildApp(cfg); err != nil {
		return err
	}
	bundle, err := bundleApp(cfg.dir)
	if err != nil {
		return err
	}
	if err := uploadBundle(cfg, bundle); err != nil {
		return err
	}
	return nil
}
