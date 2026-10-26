package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
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

const (
	deployTimeout = 5 * time.Minute
	pollInterval  = 2 * time.Second
)

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

func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "export" {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export"))
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}

func loadDotEnv(dir string) map[string]string {
	vars := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return vars
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := parseDotEnvLine(line)
		if !ok {
			continue
		}
		if _, exists := vars[key]; !exists {
			vars[key] = value
		}
	}
	return vars
}

func resolveDeployConfig(args []string) (*deployConfig, error) {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dir := fs.String("dir", ".", "application directory containing spin.toml")
	fs.String("app", "", "app name on the supervisor (default: APP, .env APP, spin.toml application name)")
	fs.String("deploy-host", "", "supervisor host, e.g. deploy.apps.example.com (default: DEPLOY_HOST, .env DEPLOY_HOST)")
	fs.String("token", "", "bearer token for the deploy API (default: SUPERVISOR_TOKEN, .env SUPERVISOR_TOKEN)")
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
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	fileVars := loadDotEnv(*dir)
	value := func(name, envKey string) string {
		if set[name] {
			return fs.Lookup(name).Value.String()
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return fileVars[envKey]
	}
	cfg := &deployConfig{dir: *dir, app: value("app", "APP"), host: value("deploy-host", "DEPLOY_HOST"), token: value("token", "SUPERVISOR_TOKEN"), skipBuild: *skipBuild}
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

type appState struct {
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Status    string    `json:"status"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

type deployResponse struct {
	status      int
	body        []byte
	contentType string
}

// fetchAppState reads the supervisor's view of the app, or nil when the app does not exist yet.
func fetchAppState(cfg *deployConfig) (*appState, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(baseURL(cfg.host) + "/apps/" + cfg.app)
	if err != nil {
		return nil, fmt.Errorf("get app state: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read app state: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get app state: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var state appState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("decode app state: %w", err)
	}
	return &state, nil
}

// newerVersion reports whether state describes a different process than before.
func newerVersion(before, state *appState) bool {
	if before == nil {
		return true
	}
	return state.PID != before.PID || !state.StartedAt.Equal(before.StartedAt)
}

// waitForDeploy polls the app until the supervisor reports a new running process, which happens only after the new
// version answered its health path. The old version keeps serving until then.
func waitForDeploy(cfg *deployConfig, before *appState) (*appState, error) {
	deadline := time.Now().Add(deployTimeout)
	var last error

	for {
		time.Sleep(pollInterval)
		state, err := fetchAppState(cfg)
		if err != nil {
			last = err
		} else if state != nil && state.Status == "running" && newerVersion(before, state) {
			return state, nil
		}
		if time.Now().After(deadline) {
			if last != nil {
				return nil, fmt.Errorf("wait for %s: %w", cfg.app, last)
			}
			if state == nil {
				return nil, fmt.Errorf("app %s did not start within %s", cfg.app, deployTimeout)
			}
			return nil, fmt.Errorf("app %s did not start within %s (status %s); the previous version is still serving if there was one", cfg.app, deployTimeout, state.Status)
		}
	}
}

func uploadBundle(cfg *deployConfig, bundle []byte) (deployResponse, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("artifact", "app.tgz")
	if err != nil {
		return deployResponse{}, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(bundle); err != nil {
		return deployResponse{}, fmt.Errorf("write form file: %w", err)
	}
	if err := form.Close(); err != nil {
		return deployResponse{}, fmt.Errorf("close form: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL(cfg.host)+"/apps/"+cfg.app, &body)
	if err != nil {
		return deployResponse{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", form.FormDataContentType())

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return deployResponse{}, fmt.Errorf("post bundle: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deployResponse{}, fmt.Errorf("read response: %w", err)
	}
	return deployResponse{status: resp.StatusCode, body: respBody, contentType: resp.Header.Get("Content-Type")}, nil
}

func printState(state *appState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
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

	before, err := fetchAppState(cfg)
	if err != nil {
		before = nil
	}

	resp, err := uploadBundle(cfg, bundle)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "deploy: %v; waiting for the new version to serve\n", err)
	case resp.status == http.StatusAccepted:
		fmt.Fprintln(os.Stderr, "deploy started; waiting for the new version to serve")
	case resp.status >= 200 && resp.status < 300:
		fmt.Print(string(resp.body))
		return nil
	case resp.status >= 500 && !strings.Contains(resp.contentType, "json"):
		// A proxy in front of the supervisor gave up waiting; the deploy carries on in the background.
		fmt.Fprintf(os.Stderr, "deploy: proxy answered %d; waiting for the new version to serve\n", resp.status)
	default:
		return fmt.Errorf("deploy failed with status %d: %s", resp.status, strings.TrimSpace(string(resp.body)))
	}

	state, err := waitForDeploy(cfg, before)
	if err != nil {
		return err
	}
	return printState(state)
}
