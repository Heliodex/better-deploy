package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, &body)
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, smallResponseLimit))
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
	client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: deployPollTimeout}
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
			time.Sleep(deployPollDelay)
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
		client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: deployPollTimeout}

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
		if len(digest) > shortHashLength {
			digest = digest[:shortHashLength]
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
}

// prepareManifest applies the deploy name and host overrides, then maps a local manifest host onto the profile's app domain.
func prepareManifest(m *manifest, cfg *deployConfig) error {
	if cfg.app != "" {
		if !validName(cfg.app) {
			return fmt.Errorf("invalid app name %q", cfg.app)
		}

		m.Name = cfg.app
	}

	if cfg.appHost != "" {
		m.Host = cfg.appHost
		return nil
	}

	applyAppDomain(m, cfg.appDomain)

	return nil
}

// buildBundle runs the app builds, precompiles the component and packs the upload artifact.
func buildBundle(cfg *deployConfig) (manifest, []byte, buildStatus, error) {
	m, err := readManifest(cfg.dir)
	if err != nil {
		return m, nil, buildStatus{}, err
	}

	if cfg.overlayEnv {
		overlayDevEnv(&m)
	}

	if err := prepareManifest(&m, cfg); err != nil {
		return m, nil, buildStatus{}, err
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

// checkDeploySecrets fails fast when the manifest references secrets the supervisor does not have, instead of failing after the build and upload.
func checkDeploySecrets(cfg *deployConfig, m manifest) error {
	var wanted []string

	for _, value := range m.Env {
		name, ok := strings.CutPrefix(value, "secret://")
		if !ok || name == "" {
			continue
		}

		wanted = append(wanted, name)
	}

	if len(wanted) == 0 {
		return nil
	}

	client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: deployPollTimeout}

	status, _, body, err := apiDo(client, http.MethodGet, "/apps/"+m.Name+"/secrets", nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(status, body); err != nil {
		return err
	}

	var result secretKeysResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("read secrets: %w", err)
	}

	have := map[string]struct{}{}
	for _, key := range result.Keys {
		have[key] = struct{}{}
	}

	var missing []string

	for _, name := range wanted {
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing secrets for app %q: %s (set with wt secrets %s set KEY)", m.Name, strings.Join(missing, ", "), m.Name)
	}

	return nil
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

	if err := prepareManifest(&m, cfg); err != nil {
		return err
	}

	if err := checkAppHostTarget(cfg.host, m.Host, m.Name, cfg.profile); err != nil {
		return err
	}

	if len(cfg.envVars) > 0 {
		vars, err := parseDeployEnv(cfg.envVars)
		if err != nil {
			return err
		}

		applyDeployEnv(&m, vars)
	}

	preflight, err := preflightDeploy(cfg)
	if err != nil {
		return err
	}

	if err := checkDeploySecrets(cfg, m); err != nil {
		return err
	}

	if err := checkDeployRoute(cfg, m); err != nil {
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

	if err := upload(cfg, m, data); err != nil {
		return err
	}

	if !cfg.verify {
		return nil
	}

	return verifyDeployment(cfg, m)
}
