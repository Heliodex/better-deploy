package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	Name            string            `json:"name"`
	Host            string            `json:"host"`
	Health          string            `json:"health"`
	Env             map[string]string `json:"env"`
	BuildComponent  string            `json:"build_component,omitempty"`
	BuildStatic     string            `json:"build_static,omitempty"`
	ComponentInputs []string          `json:"component_inputs,omitempty"`
	StaticInputs    []string          `json:"static_inputs,omitempty"`
	MemoryMB        int               `json:"memory_mb,omitempty"`
	DiskMB          int               `json:"disk_mb,omitempty"`
	MaxConcurrent   int               `json:"max_concurrency,omitempty"`
	RequestTimeout  string            `json:"request_timeout,omitempty"`
	IdleTimeout     string            `json:"idle_timeout,omitempty"`
	Contract        string            `json:"contract,omitempty"`
	Network         string            `json:"network,omitempty"`
	ServerPaths     []string          `json:"server_paths,omitempty"`
	ServerPages     []string          `json:"server_pages,omitempty"`
	Schedule        []schedule        `json:"schedule,omitempty"`
	Dev             *appDevConfig     `json:"dev,omitempty"`
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

const (
	minScheduleInterval = time.Second
	maxScheduleInterval = 24 * time.Hour
	maxServerPaths      = 32
)

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

	switch m.Contract {
	case "", "none", "valibot":
	default:
		return m, fmt.Errorf("contract must be valibot or none, got %q", m.Contract)
	}

	if m.MemoryMB < 0 || m.DiskMB < 0 {
		return m, errors.New("memory_mb and disk_mb must be 0 or more")
	}

	if err := validateIdleTimeout(m.IdleTimeout); err != nil {
		return m, err
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

	if err := validateServerPaths("server_paths", m.ServerPaths); err != nil {
		return m, err
	}

	if err := validateServerPaths("server_pages", m.ServerPages); err != nil {
		return m, err
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
		if err := normaliseDevConfig(m.Dev); err != nil {
			return m, err
		}
	}

	return m, nil
}

// defaultIdleTimeout mirrors the supervisor's scale-to-zero default for an omitted idle_timeout.
const defaultIdleTimeout = 10 * time.Minute

// validateIdleTimeout checks the optional scale-to-zero delay before the supervisor sees the bundle.
func validateIdleTimeout(value string) error {
	value = strings.TrimSpace(value)
	switch {
	case value == "", value == "0", strings.EqualFold(value, "none"), strings.EqualFold(value, "off"):
		return nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < 10*time.Second || timeout > 24*time.Hour {
		return fmt.Errorf("idle_timeout %q must be a duration between 10s and 24h, or 0/none/off to disable", value)
	}

	return nil
}

// validateServerPaths checks a field of absolute path prefixes, such as runtime-owned server paths or component-selected pages.
func validateServerPaths(field string, paths []string) error {
	if len(paths) > maxServerPaths {
		return fmt.Errorf("%s must contain at most %d prefixes", field, maxServerPaths)
	}

	for _, raw := range paths {
		value := strings.TrimSpace(raw)
		if value == "" {
			return fmt.Errorf("%s must not contain an empty prefix", field)
		}

		if !strings.HasPrefix(value, "/") {
			return fmt.Errorf("%s: %q must start with /", field, value)
		}

		if strings.ContainsAny(value, "?#\\\x00\t\r\n") {
			return fmt.Errorf("%s: %q contains a query, fragment, backslash or whitespace", field, value)
		}

		clean := path.Clean(value)
		if clean != value && clean != strings.TrimSuffix(value, "/") {
			return fmt.Errorf("%s: %q must be a clean absolute path", field, value)
		}
	}

	return nil
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
var defaultSupervisorProxy = []string{"/v1/events", "/_wt/blobs", "/_wt/identity"}

// normaliseDevConfig validates dev paths and fills the default proxy prefixes.
func normaliseDevConfig(dev *appDevConfig) error {
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
		return errors.New("dev.port must be between 1 and 65535, or 0 for a free port")
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
