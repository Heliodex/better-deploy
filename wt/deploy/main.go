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
	Name            string            `json:"name"`
	Host            string            `json:"host"`
	Health          string            `json:"health"`
	Env             map[string]string `json:"env"`
	Build           string            `json:"build"`
	BuildComponent  string            `json:"build_component,omitempty"`
	BuildStatic     string            `json:"build_static,omitempty"`
	ComponentInputs []string          `json:"component_inputs,omitempty"`
	StaticInputs    []string          `json:"static_inputs,omitempty"`
	MemoryMB        int               `json:"memory_mb,omitempty"`
	DiskMB          int               `json:"disk_mb,omitempty"`
	MaxConcurrent   int               `json:"max_concurrency,omitempty"`
	RequestTimeout  string            `json:"request_timeout,omitempty"`
	Network         string            `json:"network,omitempty"`
	Schedule        []schedule        `json:"schedule,omitempty"`
	Dev             *appDevConfig     `json:"dev,omitempty"`
}

// componentBuildCommand selects the component phase, falling back to the legacy single build command.
func (m manifest) componentBuildCommand() string {
	if strings.TrimSpace(m.BuildComponent) != "" {
		return m.BuildComponent
	}
	return strings.TrimSpace(m.Build)
}

// staticBuildCommand returns the explicit static phase. A legacy single build is handled by prepareBuilds.
func (m manifest) staticBuildCommand() string {
	return strings.TrimSpace(m.BuildStatic)
}

// legacyBuild reports a manifest that only has the original single build command and cannot separate component and static work.
func (m manifest) legacyBuild() bool {
	return strings.TrimSpace(m.BuildComponent) == "" && strings.TrimSpace(m.BuildStatic) == "" && strings.TrimSpace(m.Build) != ""
}

// appDevConfig selects the optional SvelteKit development loop for an app.
type appDevConfig struct {
	// Web is the frontend directory relative to the app; default web.
	Web string `json:"web,omitempty"`
	// Command starts the frontend dev server; default bun run dev.
	Command string `json:"command,omitempty"`
	// Build rebuilds only the component; default app.json build_component.
	Build string `json:"build,omitempty"`
	// Proxy lists app path prefixes forwarded to the supervisor; default /api.
	Proxy []string `json:"proxy,omitempty"`
	// SupervisorProxy lists supervisor-owned app-host prefixes; defaults to the event, blob and identity routes. Set [] to disable them.
	SupervisorProxy []string `json:"supervisor_proxy,omitempty"`
	// Watch lists extra files or directories that trigger a component rebuild.
	Watch []string `json:"watch,omitempty"`
	// Port fixes the frontend dev port; default is a free port.
	Port int `json:"port,omitempty"`
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
	CompiledAt time.Time `json:"compiled_at"`
	// Epoch records epoch-interruption instrumentation, which per-request wasmtime timeouts require.
	Epoch      bool   `json:"epoch_interruption,omitempty"`
	WasmSHA256 string `json:"wasm_sha256,omitempty"`
	WasmBytes  int64  `json:"wasm_bytes,omitempty"`
	// CPU records the feature policy; compile always passes --target, so it is the baseline set.
	CPU string `json:"cpu,omitempty"`
}

const (
	minScheduleInterval = time.Second
	maxScheduleInterval = 24 * time.Hour
)

type deployConfig struct {
	dir, app, host, token, wasmtime, target, appHost, out   string
	buildComponentCommand                                   string
	skipBuild, skipCompile, skipStatic, jsonOut, forceBuild bool
	targetConfigured, noPreflight                           bool
	start                                                   time.Time
}

// uploadProgress prints a coarse percentage while a bundle uploads, without buffering it a second time.
type uploadProgress struct {
	reader      io.Reader
	total, done int64
	next        int64
	last        time.Time
}

func (u *uploadProgress) Read(p []byte) (int, error) {
	count, err := u.reader.Read(p)
	u.done += int64(count)
	if u.total <= 0 {
		return count, err
	}
	if u.next == 0 {
		u.next = u.total / 4
		u.last = time.Now()
	}
	if u.done >= u.next || err == io.EOF {
		percent := u.done * 100 / u.total
		fmt.Fprintf(os.Stderr, "upload: %d%% (%d/%d bytes)\n", percent, u.done, u.total)
		u.next = u.done + u.total/4
	}
	return count, err
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// validName reports whether name is a lowercase DNS label no longer than 63 characters.
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

// readManifest loads and validates app.json, defaulting the name to the directory name.
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
	for i, value := range m.ComponentInputs {
		if !validInputPath(value) {
			return m, fmt.Errorf("component_inputs[%d]: %q must be a relative file or directory", i, value)
		}
	}
	for i, value := range m.StaticInputs {
		if !validInputPath(value) {
			return m, fmt.Errorf("static_inputs[%d]: %q must be a relative file or directory", i, value)
		}
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
	if m.Dev != nil {
		if err := normalizeDevConfig(m.Dev); err != nil {
			return m, err
		}
	}
	return m, nil
}

// validAppRelative keeps dev paths inside the app directory.
func validAppRelative(value string) bool {
	if value == "" {
		return true
	}
	if filepath.IsAbs(value) {
		return false
	}
	clean := filepath.Clean(value)
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// validInputPath accepts a relative build input, including a sibling SDK, but rejects the app root and absolute paths.
func validInputPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || strings.HasPrefix(value, "~") {
		return false
	}
	clean := filepath.Clean(value)
	return clean != "." && clean != ".." && clean != string(filepath.Separator)
}

// defaultSupervisorProxy lists the supervisor-owned app-host routes the Vite dev server must forward in addition to the app API.
var defaultSupervisorProxy = []string{"/v1/events", "/_wt/events", "/_wt/blobs", "/_wt/identity"}

// normalizeDevConfig validates dev paths and fills the default proxy prefixes.
func normalizeDevConfig(dev *appDevConfig) error {
	if !validAppRelative(dev.Web) {
		return fmt.Errorf("dev.web %q must stay inside the app directory", dev.Web)
	}
	for _, path := range dev.Watch {
		if !validAppRelative(path) {
			return fmt.Errorf("dev.watch %q must stay inside the app directory", path)
		}
	}
	if len(dev.Proxy) == 0 {
		dev.Proxy = []string{"/api"}
	}
	if dev.SupervisorProxy == nil {
		dev.SupervisorProxy = append([]string(nil), defaultSupervisorProxy...)
	}
	if err := validateDevPrefixes("dev.proxy", dev.Proxy); err != nil {
		return err
	}
	if err := validateDevPrefixes("dev.supervisor_proxy", dev.SupervisorProxy); err != nil {
		return err
	}
	if dev.Port < 0 || dev.Port > 65535 {
		return fmt.Errorf("dev.port must be between 1 and 65535, or 0 for a free port")
	}
	return nil
}

// validateDevPrefixes checks absolute proxy prefixes and rejects traversal.
func validateDevPrefixes(name string, prefixes []string) error {
	for _, prefix := range prefixes {
		if !strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "..") {
			return fmt.Errorf("%s %q must be an absolute path prefix such as /api", name, prefix)
		}
	}
	return nil
}

// resolveConfig builds the deploy config from the config file, environment and deploy flags.
func resolveConfig(args []string) (*deployConfig, error) {
	configPath := configArg(args)
	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	app := flags.String("app", "", "app name (default: app.json name or directory name)")
	host := flags.String("host", firstNonEmpty(env("WT_DEPLOY_HOST", ""), fileCfg.Host), "supervisor URL, e.g. http://127.0.0.1:8080")
	token := flags.String("token", firstNonEmpty(env("WT_TOKEN", ""), fileCfg.Token), "deploy bearer token")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME", ""), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	targetDefault := firstNonEmpty(env("WT_WASMTIME_TARGET", ""), fileCfg.Target)
	targetConfigured := targetDefault != ""
	if targetDefault == "" {
		targetDefault = "x86_64-unknown-linux-musl"
	}
	target := flags.String("target", targetDefault, "wasmtime compile target (default: supervisor target or x86_64-unknown-linux-musl)")
	appHost := flags.String("app-host", fileCfg.AppHost, "override the app host in the uploaded app.json")
	config := flags.String("config", configPath, "config file (default ~/.config/wt/config.json)")
	jsonOut := flags.Bool("json", false, "print only the raw API response")
	forceBuild := flags.Bool("force-build", false, "rebuild the component and static phases even when their inputs are unchanged")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build commands")
	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")
	noPreflight := flags.Bool("no-preflight", false, "skip the supervisor capability check and target discovery")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *target != targetDefault {
		targetConfigured = true
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "target" {
			targetConfigured = true
		}
	})
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *config != configPath {
		fileCfg, err = loadFileConfig(*config)
		if err != nil {
			return nil, err
		}
		if *host == "" {
			*host = fileCfg.Host
		}
		if *token == "" {
			*token = fileCfg.Token
		}
	}
	if *host == "" {
		return nil, fmt.Errorf("set --host, WT_DEPLOY_HOST or host in the config file")
	}
	return &deployConfig{
		dir:              *dir,
		app:              *app,
		host:             *host,
		token:            *token,
		wasmtime:         *wasmtime,
		target:           *target,
		skipBuild:        *skipBuild,
		skipCompile:      *skipCompile,
		forceBuild:       *forceBuild,
		appHost:          *appHost,
		jsonOut:          *jsonOut,
		targetConfigured: targetConfigured,
		noPreflight:      *noPreflight,
	}, nil
}

// runBuildPhase runs one named app.json build phase with the app directory as its working directory.
func runBuildPhase(cfg *deployConfig, phase, command string) error {
	if cfg.skipBuild {
		return nil
	}
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("app.json has no %s build command; set one or pass --skip-build", phase)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = cfg.dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s build: %w", phase, err)
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

// bundle packs app.json, the component files and static output into a gzipped tarball for upload.
func bundle(dir string, m manifest, includeComponent bool) ([]byte, error) {
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
	if includeComponent {
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

// baseURL adds https when host has no scheme and strips a trailing slash.
func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

// upload posts the bundle as a multipart deployment and waits for a queued deployment to finish.
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

	url := baseURL(cfg.host) + "/apps/" + m.Name + "/deployments"
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	size := int64(body.Len())
	req.ContentLength = size
	progress := &uploadProgress{reader: bytes.NewReader(body.Bytes()), total: size}
	req.Body = io.NopCloser(progress)

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
	if resp.StatusCode != http.StatusAccepted {
		return printDeployResult(cfg, m.Name, respBody)
	}
	location := resp.Header.Get("Location")
	var queued deploymentState
	if err := json.Unmarshal(respBody, &queued); err != nil {
		return fmt.Errorf("read deployment: %w", err)
	}
	if location == "" {
		location = "/apps/" + m.Name + "/deployments/" + queued.ID
	}
	if queued.ID == "" {
		queued.ID = filepath.Base(location)
	}
	return waitDeploy(cfg, m.Name, location)
}

// deploymentState is the subset of a deployment resource the CLI needs.
type deploymentState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Mode   string `json:"mode"`
	Digest string `json:"digest"`
	Error  string `json:"error"`
}

// waitDeploy polls a queued deployment until it succeeds or fails, then prints the app.
func waitDeploy(cfg *deployConfig, name, location string) error {
	client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: 30 * time.Second}
	for {
		status, _, body, err := apiDo(client, http.MethodGet, location, nil, "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("deployment status %d: %s", status, strings.TrimSpace(string(body)))
		}
		var state deploymentState
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("read deployment status: %w", err)
		}
		switch state.Status {
		case "queued", "running":
			time.Sleep(250 * time.Millisecond)
		case "success":
			return printDeployResult(cfg, name, nil)
		default:
			if state.Error != "" {
				return fmt.Errorf("deploy %s: %s", state.Status, state.Error)
			}
			return fmt.Errorf("deploy %s", state.Status)
		}
	}
}

// printDeployResult prints the app info from a fresh GET, or the provided response body.
func printDeployResult(cfg *deployConfig, name string, body []byte) error {
	if body == nil {
		client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: 30 * time.Second}
		status, _, data, err := apiDo(client, http.MethodGet, "/apps/"+name, nil, "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("app info status %d: %s", status, strings.TrimSpace(string(data)))
		}
		body = data
	}
	if cfg.jsonOut {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	var info struct {
		Name          string `json:"name"`
		Host          string `json:"host"`
		Status        string `json:"status"`
		Ready         bool   `json:"ready"`
		CurrentDigest string `json:"currentDigest"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	if info.Name == "" {
		info.Name = name
	}
	fmt.Printf("deployed %s\n", info.Name)
	if info.Host != "" {
		fmt.Printf("  url: %s\n", appURLForHost(cfg.host, info.Host))
	}
	if info.CurrentDigest != "" {
		digest := info.CurrentDigest
		if len(digest) > 12 {
			digest = digest[:12]
		}
		fmt.Printf("  digest: %s\n", digest)
	}
	if info.Status != "" {
		fmt.Printf("  status: %s", info.Status)
		if info.Ready {
			fmt.Print(" (ready)")
		}
		fmt.Println()
	}
	if !cfg.start.IsZero() {
		fmt.Printf("  time: %s\n", time.Since(cfg.start).Round(time.Millisecond))
	}
	return nil
	return nil
}

// buildBundle runs the app builds, precompiles the component and packs the upload artifact.
func buildBundle(cfg *deployConfig) (manifest, []byte, buildStatus, error) {
	m, err := readManifest(cfg.dir)
	if err != nil {
		return m, nil, buildStatus{}, err
	}
	if cfg.app != "" {
		if !validName(cfg.app) {
			return m, nil, buildStatus{}, fmt.Errorf("invalid app name %q", cfg.app)
		}
		m.Name = cfg.app
	}
	if cfg.appHost != "" {
		m.Host = cfg.appHost
	}
	status, err := prepareBuilds(cfg, m)
	if err != nil {
		return m, nil, status, err
	}
	data, err := bundle(cfg.dir, m, true)
	if err != nil {
		return m, nil, status, err
	}
	return m, data, status, nil
}

// deploy builds the bundle and uploads it, omitting the component when the supervisor already runs the same wasm.
func deploy(args []string) error {
	cfg, err := resolveConfig(args)
	if err != nil {
		return err
	}
	cfg.start = time.Now()
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
	preflight, err := preflightDeploy(cfg)
	if err != nil {
		return err
	}
	if preflight.TargetFromServer {
		fmt.Fprintf(os.Stderr, "target: %s (from supervisor)\n", cfg.target)
	}
	status, err := prepareBuilds(cfg, m)
	if err != nil {
		return err
	}
	printBuildStatus(status)

	keepComponent := false
	if !cfg.forceBuild && status.WasmDigest != "" {
		keepComponent = keepRunningComponent(false, status, serverDigest(cfg, m.Name))
	}
	if keepComponent {
		fmt.Fprintln(os.Stderr, "component: already running; uploading static only")
	} else if status.WasmDigest != "" {
		fmt.Fprintln(os.Stderr, "component: uploading")
		_, cwasmErr := os.Stat(filepath.Join(cfg.dir, "main.cwasm"))
		printRuntimePlan(preflight, cwasmErr == nil)
	}
	data, err := bundle(cfg.dir, m, !keepComponent)
	if err != nil {
		return err
	}
	return upload(cfg, m, data)
}

// resolveBundleConfig parses the flags for building a bundle without a supervisor.
func resolveBundleConfig(args []string) (*deployConfig, error) {
	configPath := configArg(args)
	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}
	flags := flag.NewFlagSet("bundle", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME", ""), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	target := flags.String("target", firstNonEmpty(env("WT_WASMTIME_TARGET", ""), fileCfg.Target, "x86_64-unknown-linux-musl"), "wasmtime compile target")
	out := flags.String("out", "", "output file (default <name>.tar.gz)")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build commands")
	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")
	forceBuild := flags.Bool("force-build", false, "rebuild the component and static phases even when their inputs are unchanged")
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
		forceBuild:  *forceBuild,
		out:         *out,
	}, nil
}

// runBundle builds the deploy artifact into a file instead of uploading it, for CI or manual upload.
func runBundle(args []string) error {
	cfg, err := resolveBundleConfig(args)
	if err != nil {
		return err
	}
	m, data, status, err := buildBundle(cfg)
	if err != nil {
		return err
	}
	printBuildStatus(status)
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
	case "bundle", "build":
		err = runBundle(args)
	case "dev":
		err = runDev(args)
	case "new":
		err = runNew(args)
	case "apps":
		err = runApps(args)
	case "logs":
		err = runLogs(args)
	case "jobs":
		err = runJobs(args)
	case "rollback":
		err = runRollback(args)
	case "backup":
		err = runBackup(args)
	case "config":
		err = runConfig(args)
	case "open":
		err = runOpen(args)
	case "completion":
		err = runCompletion(args)
	case "help", "-h", "--help":
		fmt.Println("usage: wt <command> [flags]")
		fmt.Println("       wt apps [--host URL] [--token TOKEN] [--json]")
		fmt.Println("       wt logs <app> [--tail N] [--level LEVEL] [--source app|supervisor] [--q TEXT] [--follow]")
		fmt.Println("       wt jobs <app> <list|show|enqueue|cancel|retry> ...")
		fmt.Println("       wt deploy [--dir DIR] [--host URL] [--token TOKEN] [--app-host HOST]")
		fmt.Println("       wt rollback <app> <deployment-id> [--backup SNAPSHOT]")
		fmt.Println("       wt backup <list|create|restore|download|import> <app> ...")
		fmt.Println("       wt config <show|set> <app> ...")
		fmt.Println("       wt open <app>")
		fmt.Println("       wt build --dir DIR --out app.tar.gz")
		fmt.Println("       wt dev [--dir DIR] [--port 8080] [--watch=false]")
		fmt.Println("       wt new --dir PATH [--sdk PATH|embedded] [--module MODULE]")
		fmt.Println("       wt completion <bash|zsh|fish>")
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}
