// App configuration: environment, flags and app.json validation.

package main

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func configFromEnv() config {
	return config{
		appsDir:          env("WT_APPS_DIR", "./apps"),
		stateDir:         env("WT_STATE_DIR", "./state"),
		cacheDir:         env("WT_CACHE_DIR", ""),
		cacheMaxBytes:    envInt64("WT_CACHE_MAX_BYTES", defaultCacheMaxBytes),
		listen:           env("WT_LISTEN", "127.0.0.1:8080"),
		storageListen:    env("WT_STORAGE_LISTEN", "127.0.0.1:0"),
		domain:           env("WT_APPS_DOMAIN", "localhost"),
		wasmtime:         env("WT_WASMTIME", "wasmtime"),
		wasmtimeCompiler: env("WT_WASMTIME_COMPILER", env("WT_WASMTIME", "wasmtime")),
		wasmtimeTarget:   env("WT_WASMTIME_TARGET", "x86_64-unknown-linux-musl"),
		token:            os.Getenv("WT_TOKEN"),
		tlsCert:          os.Getenv("WT_TLS_CERT"),
		tlsKey:           os.Getenv("WT_TLS_KEY"),
	}
}

// sameRuntimeConfig compares the settings that require a new process, ignoring build and display metadata.
func sameRuntimeConfig(a, b appConfig) bool {
	if a.Host != b.Host || a.Health != b.Health {
		return false
	}
	if !maps.Equal(a.Env, b.Env) {
		return false
	}
	if a.MemoryMB != b.MemoryMB || a.DiskMB != b.DiskMB || a.MaxConcurrent != b.MaxConcurrent {
		return false
	}
	if a.requestTimeout != b.requestTimeout || a.Network != b.Network {
		return false
	}
	return slices.Equal(a.Schedule, b.Schedule)
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

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, " /:@") || strings.HasPrefix(host, ".") ||
		strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func defaultHost(name, domain string) string {
	if domain != "" {
		return name + "." + domain
	}
	return name + ".localhost"
}

func validateAppHost(host string) error {
	if !validHost(host) {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

// parseRequestTimeout validates an optional per-request deadline. Empty or zero means no supervisor deadline.
func parseRequestTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < time.Second || timeout > 24*time.Hour {
		return 0, fmt.Errorf("request_timeout %q must be a duration between 1s and 24h", value)
	}
	return timeout, nil
}

// parseScheduleInterval validates an entry's period and reports how often it fires.
func parseScheduleInterval(entry scheduleConfig) (time.Duration, error) {
	every, err := time.ParseDuration(entry.Every)
	if err != nil {
		return 0, fmt.Errorf("invalid every %q: use a duration such as 10s", entry.Every)
	}
	if every < minScheduleInterval || every > maxScheduleInterval {
		return 0, fmt.Errorf("every %q must be between %s and %s", entry.Every, minScheduleInterval, maxScheduleInterval)
	}
	return every, nil
}

// normalizeAppConfig fills in defaults and validates an app's runtime config.
func normalizeAppConfig(cfg appConfig, name, domain string) (appConfig, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = name
	}
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = defaultHost(name, domain)
	}
	cfg.Host = strings.ToLower(strings.TrimSpace(cfg.Host))
	if err := validateAppHost(cfg.Host); err != nil {
		return cfg, err
	}
	if cfg.Health == "" {
		cfg.Health = "/"
	}
	if !strings.HasPrefix(cfg.Health, "/") {
		return cfg, fmt.Errorf("health must start with /")
	}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	for key := range cfg.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return cfg, fmt.Errorf("invalid env key %q", key)
		}
	}
	if cfg.MemoryMB < 0 {
		return cfg, fmt.Errorf("memory_mb must be 0 or more")
	}
	if cfg.DiskMB < 0 {
		return cfg, fmt.Errorf("disk_mb must be 0 or more")
	}
	if cfg.MaxConcurrent < 0 {
		return cfg, fmt.Errorf("max_concurrency must be 0 or more")
	}
	switch cfg.Network {
	case "", "inherit":
		cfg.Network = "inherit"
	case "none":
	default:
		return cfg, fmt.Errorf("network must be inherit or none")
	}
	timeout, err := parseRequestTimeout(cfg.RequestTimeout)
	if err != nil {
		return cfg, err
	}
	cfg.requestTimeout = timeout
	for i := range cfg.Schedule {
		entry := &cfg.Schedule[i]
		if !strings.HasPrefix(entry.Path, "/") {
			return cfg, fmt.Errorf("schedule[%d]: path must start with /", i)
		}
		if _, err := parseScheduleInterval(*entry); err != nil {
			return cfg, fmt.Errorf("schedule[%d]: %w", i, err)
		}
	}
	return cfg, nil
}

func readAppConfig(dir, name, domain string) (appConfig, error) {
	cfg := appConfig{Name: name, Env: map[string]string{}}

	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("app.json: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return cfg, fmt.Errorf("read app.json: %w", err)
	}
	return normalizeAppConfig(cfg, name, domain)
}
