package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type inspectDeployment struct {
	ID         string    `json:"id"`
	DeployedAt time.Time `json:"deployedAt"`
	Mode       string    `json:"mode"`
	Digest     string    `json:"digest"`
	Runtime    string    `json:"runtime"`
	Target     string    `json:"target"`
}

// inspectReport is the machine-readable view printed by --json.
type inspectReport struct {
	Supervisor supervisorCapabilities `json:"supervisor"`
	App        routeAppInfo           `json:"app"`
	URL        string                 `json:"url"`
	Route      routeReport            `json:"route"`
	Config     map[string]any         `json:"config"`
	Deployment *inspectDeployment     `json:"deployment,omitempty"`
	Manifest   *manifestComparison    `json:"manifest,omitempty"`
}

type manifestComparison struct {
	Path        string   `json:"path"`
	Differences []string `json:"differences,omitempty"`
}

// inspectConfig fetches an app's runtime config as an untyped document so new fields stay visible.
func inspectConfig(cfg clientOptions, name string) (map[string]any, error) {
	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/config", nil, "")
	if err != nil {
		return nil, err
	}

	if err := checkAPI(status, body); err != nil {
		return nil, err
	}

	config := map[string]any{}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, err
	}

	return config, nil
}

// inspectLatestDeployment returns the newest deployment entry, if any.
func inspectLatestDeployment(cfg clientOptions, name string) (inspectDeployment, bool, error) {
	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/deployments?limit=1", nil, "")
	if err != nil {
		return inspectDeployment{}, false, err
	}

	if err := checkAPI(status, body); err != nil {
		return inspectDeployment{}, false, err
	}

	var entries []inspectDeployment
	if err := json.Unmarshal(body, &entries); err != nil {
		return inspectDeployment{}, false, err
	}

	if len(entries) == 0 {
		return inspectDeployment{}, false, nil
	}

	return entries[0], true, nil
}

// valueText renders one config or manifest value for comparison.
func valueText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}

		return "false"
	case float64:
		return fmt.Sprintf("%v", typed)
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}

	return string(data)
}

// envKeys extracts sorted keys from a local app.json env object.
func envKeys(value any) []string {
	env, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// compareManifest reports the local app.json fields that differ from the effective runtime config.
func compareManifest(dir, name string, config map[string]any) *manifestComparison {
	path := filepath.Join(dir, "app.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var local map[string]any
	if err := json.Unmarshal(data, &local); err != nil {
		return &manifestComparison{Path: path, Differences: []string{"cannot parse app.json: " + err.Error()}}
	}

	if manifestName := valueText(local["name"]); manifestName != "" && manifestName != name {
		return &manifestComparison{Path: path, Differences: []string{fmt.Sprintf("name: manifest %s, inspected %s", manifestName, name)}}
	}

	comparison := &manifestComparison{Path: path}

	for _, key := range []string{"host", "health", "memory_mb", "disk_mb", "max_concurrency", "request_timeout", "idle_timeout", "network", "identity", "server_paths", "server_pages"} {
		localValue, ok := local[key]
		if !ok {
			continue
		}

		if key == "idle_timeout" {
			if sameIdleTimeout(localValue, config[key]) {
				continue
			}
		}

		if got, want := valueText(localValue), valueText(config[key]); got != want {
			comparison.Differences = append(comparison.Differences, fmt.Sprintf("%s: manifest %s, runtime %s", key, got, want))
		}
	}

	if env, ok := local["env"]; ok {
		localKeys := strings.Join(envKeys(env), ",")
		configKeys := ""

		if keys, ok := config["env_keys"].([]any); ok {
			values := make([]string, 0, len(keys))
			for _, key := range keys {
				values = append(values, valueText(key))
			}

			sort.Strings(values)
			configKeys = strings.Join(values, ",")
		}

		if localKeys != configKeys {
			comparison.Differences = append(comparison.Differences, fmt.Sprintf("env keys: manifest %s, runtime %s", localKeys, configKeys))
		}
	}

	return comparison
}

// sameIdleTimeout compares two idle_timeout values by duration, so "15m" and "900s" match and disabled spellings agree.
func sameIdleTimeout(local, runtime any) bool {
	left, leftOK := idleTimeoutValue(valueText(local))
	right, rightOK := idleTimeoutValue(valueText(runtime))

	return leftOK && rightOK && left == right
}

// idleTimeoutValue parses a manifest or runtime idle_timeout, reporting false for an invalid duration.
func idleTimeoutValue(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	switch {
	case value == "", value == "0", strings.EqualFold(value, "none"), strings.EqualFold(value, "off"):
		return 0, true
	}

	timeout, err := time.ParseDuration(value)

	return timeout, err == nil
}

// printInspect prints the effective app view.
func printInspect(report inspectReport) {
	fmt.Printf("supervisor: %s\n", report.Supervisor.displayVersion())

	if report.Supervisor.Wasmtime != "" {
		fmt.Printf("wasmtime: %s\n", report.Supervisor.Wasmtime)
	}

	if report.Supervisor.WasmtimeTarget != "" {
		fmt.Printf("target: %s\n", report.Supervisor.WasmtimeTarget)
	}

	fmt.Printf("compiler: %v\n", report.Supervisor.Compiler)
	fmt.Printf("app: %s\n", report.App.Name)
	fmt.Printf("host: %s\n", report.App.Host)
	fmt.Printf("url: %s\n", report.URL)
	fmt.Printf("status: %s (ready %v)\n", report.App.Status, report.App.Ready)

	if report.App.CurrentDigest != "" {
		digest := report.App.CurrentDigest
		if len(digest) > shortHashLength {
			digest = digest[:shortHashLength]
		}

		fmt.Printf("digest: %s\n", digest)
	}

	if report.Deployment != nil {
		fmt.Printf("deployment: %s (%s, %s)\n", jobTime(report.Deployment.DeployedAt), report.Deployment.Mode, report.Deployment.Runtime)

		if report.Deployment.Target != "" {
			fmt.Printf("deployed target: %s\n", report.Deployment.Target)
		}
	}

	configKeys := []string{
		"health", "memory_mb", "disk_mb", "max_concurrency", "job_concurrency",
		"request_timeout", "startup_timeout", "idle_timeout", "network", "identity", "maintenance",
		"allow_callers", "depends_on", "server_paths", "egress", "schedule", "restart_required",
	}

	fmt.Println("config:")

	for _, key := range configKeys {
		if value, ok := report.Config[key]; ok {
			fmt.Printf("  %s: %s\n", key, valueText(value))
		}
	}

	if keys, ok := report.Config["env_keys"].([]any); ok {
		fmt.Printf("  env_keys: %s\n", joinValues(keys))
	}

	if keys, ok := report.Config["secret_keys"].([]any); ok {
		fmt.Printf("  secret_keys: %s\n", joinValues(keys))
	}

	fmt.Printf("route: routed %v (HTTP %d)\n", report.Route.Routed, report.Route.HTTPStatus)

	if report.Manifest != nil {
		if len(report.Manifest.Differences) == 0 {
			fmt.Printf("manifest: %s (matches compared fields)\n", report.Manifest.Path)
		} else {
			fmt.Printf("manifest: %s (%d difference(s))\n", report.Manifest.Path, len(report.Manifest.Differences))

			for _, difference := range report.Manifest.Differences {
				fmt.Printf("  %s\n", difference)
			}
		}
	}
}

// joinValues renders a JSON array of strings.
func joinValues(values []any) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, valueText(value))
	}

	if len(out) == 0 {
		return "(none)"
	}

	return strings.Join(out, ", ")
}

// runInspect prints the effective configuration, route, deployment and optional local manifest comparison.
func runInspect(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	dir := "."
	name := ""

	for index := 0; index < len(rest); index++ {
		arg := rest[index]
		switch {
		case arg == "--dir" && index+1 < len(rest):
			index++
			dir = rest[index]
		case strings.HasPrefix(arg, "--dir="):
			dir = strings.TrimPrefix(arg, "--dir=")
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unexpected flag %q", arg)
		default:
			if name != "" {
				return fmt.Errorf("unexpected argument %q", arg)
			}

			name = arg
		}
	}

	if name == "" {
		return errors.New("usage: wt inspect <app> [--dir DIR]")
	}

	if !validName(name) {
		return fmt.Errorf("invalid app name %q", name)
	}

	info, err := routeApp(cfg, name)
	if err != nil {
		return err
	}

	config, err := inspectConfig(cfg, name)
	if err != nil {
		return err
	}

	caps, err := fetchCapabilities(clientOptions{host: cfg.host, token: cfg.token, timeout: 10 * time.Second})
	if err != nil {
		return err
	}

	deployment, hasDeployment, err := inspectLatestDeployment(cfg, name)
	if err != nil {
		return err
	}

	var reportDeployment *inspectDeployment
	if hasDeployment {
		reportDeployment = &deployment
	}

	route := probeRoute(cfg.host, info.Host, defaultRoutePath, 8*time.Second)
	route.App = info.Name

	report := inspectReport{
		Supervisor: caps,
		App:        info,
		URL:        appURLForHost(cfg.host, info.Host),
		Route:      route,
		Config:     config,
		Deployment: reportDeployment,
		Manifest:   compareManifest(dir, name, config),
	}

	if cfg.jsonOut {
		printRawOrJSONJSON(report)

		return nil
	}

	printInspect(report)

	return nil
}
