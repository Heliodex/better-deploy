// App configuration: app.json validation, defaults and normalisation.

package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	defaultStartupTimeout = 15 * time.Second
	minStartupTimeout     = time.Second
	maxStartupTimeout     = 10 * time.Minute

	// defaultIdleTimeout is how long an app may go without component traffic before it sleeps. Empty idle_timeout means this; 0/none/off disable it.
	defaultIdleTimeout = 10 * time.Minute
	minIdleTimeout     = 10 * time.Second
	maxIdleTimeout     = 24 * time.Hour

	tenantTrusted   = "trusted"
	tenantUntrusted = "untrusted"

	defaultCPUMillicores = 1000
	defaultPidsMax       = 128
	defaultIOWeight      = 100
	maxIOWeight          = 10000
	maxMemoryMB          = 1 << 20
	maxDiskMB            = 1 << 20
	maxCPUMillicores     = 1_000_000
	maxPidsMax           = 1_000_000

	// Untrusted tenancy defaults are deliberately finite: an app with omitted fields must not inherit unbounded CPU, memory, disk or time.
	untrustedDefaultRequestTimeout = "30s"
	untrustedDefaultMemoryMB       = 256
	untrustedDefaultDiskMB         = 256
	untrustedDefaultNetwork        = "storage"
	maxAllowCallers                = 128
	maxDependsOn                   = 32
	maxServerPaths                 = 32
	maxEgressRules                 = 128
)

// untrusted reports whether apps are treated as hostile tenants that need enforced isolation.
func (c config) untrusted() bool {
	return c.tenantMode == tenantUntrusted
}

// validateTenantConfig rejects an unsafe tenant mode. Untrusted tenancy has no tokenless path, even on loopback.
func validateTenantConfig(cfg config) error {
	switch cfg.tenantMode {
	case "", tenantTrusted:
		return nil
	case tenantUntrusted:
		if strings.TrimSpace(cfg.token) == "" {
			return fmt.Errorf("WT_TOKEN is required when WT_TENANT_MODE=%s", tenantUntrusted)
		}

		return nil
	default:
		return fmt.Errorf("WT_TENANT_MODE must be %s or %s, got %q", tenantTrusted, tenantUntrusted, cfg.tenantMode)
	}
}

// validateIsolationConfig makes the namespace and seccomp boundaries mandatory in untrusted mode.
// They can be disabled only for trusted apps, where the operator has already accepted that the code is not hostile.
func validateIsolationConfig(cfg config) error {
	if !cfg.untrusted() {
		return nil
	}

	if !cfg.appNamespaces {
		return fmt.Errorf("WT_APP_NAMESPACES=off is not allowed in %s mode", tenantUntrusted)
	}

	if !cfg.appSeccomp {
		return fmt.Errorf("WT_APP_SECCOMP=off is not allowed in %s mode", tenantUntrusted)
	}

	return nil
}

// validateStorageTempDir keeps the SQLite temp directory from being the state or apps root, or a parent of either.
// The temp directory is cleaned of SQLite's etilqs_ files at startup and its size is counted, so pointing it at a shared or broad directory would be misleading.
func validateStorageTempDir(tmp, state, apps string) error {
	tmp = filepath.Clean(tmp)
	state = filepath.Clean(state)
	apps = filepath.Clean(apps)

	if tmp == string(filepath.Separator) {
		return errors.New("storage temp dir must not be the filesystem root")
	}

	if tmp == state || tmp == apps {
		return fmt.Errorf("storage temp dir %s must be a dedicated directory, not the state or apps directory", tmp)
	}

	separator := string(filepath.Separator)
	if strings.HasPrefix(state, tmp+separator) || strings.HasPrefix(apps, tmp+separator) {
		return fmt.Errorf("storage temp dir %s must not be a parent of the state or apps directory", tmp)
	}

	return nil
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

	if a.CPUMillicores != b.CPUMillicores || a.PidsMax != b.PidsMax || a.IOWeight != b.IOWeight {
		return false
	}

	if a.requestTimeout != b.requestTimeout || a.Network != b.Network {
		return false
	}

	if a.StartupTimeout != b.StartupTimeout || a.HealthInterval != b.HealthInterval {
		return false
	}

	if a.HealthFailures != b.HealthFailures || a.RestartOnUnhealthy != b.RestartOnUnhealthy {
		return false
	}

	if !slices.Equal(a.Egress, b.Egress) {
		return false
	}

	return slices.Equal(a.Schedule, b.Schedule)
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

// defaultHost returns <name>.<domain>, or <name>.localhost when no domain is set.
func defaultHost(name, domain string) string {
	if domain != "" {
		return name + "." + domain
	}

	return name + ".localhost"
}

// validateAppSettings checks the runtime limits and fills in the non-tenant defaults that have no mode dependency.
func validateAppSettings(cfg *appConfig) error {
	if cfg.JobConcurrency == 0 {
		cfg.JobConcurrency = jobDefaultConcurrency
	}

	if cfg.JobConcurrency < 1 || cfg.JobConcurrency > jobMaxConcurrency {
		return fmt.Errorf("job_concurrency must be between 1 and %d", jobMaxConcurrency)
	}

	if cfg.Health == "" {
		cfg.Health = defaultHealthPath
	}

	if !strings.HasPrefix(cfg.Health, "/") {
		return errors.New("health must start with /")
	}

	serverPaths, err := normaliseServerPaths(cfg.ServerPaths)
	if err != nil {
		return err
	}

	cfg.ServerPaths = serverPaths

	serverPages, err := normaliseServerPages(cfg.ServerPages)
	if err != nil {
		return err
	}

	cfg.ServerPages = serverPages

	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}

	for key := range cfg.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("invalid env key %q", key)
		}
	}

	if cfg.MemoryMB < 0 || cfg.MemoryMB > maxMemoryMB {
		return fmt.Errorf("memory_mb must be between 0 and %d", maxMemoryMB)
	}

	if cfg.DiskMB < 0 || cfg.DiskMB > maxDiskMB {
		return fmt.Errorf("disk_mb must be between 0 and %d", maxDiskMB)
	}

	if cfg.MaxConcurrent < 0 {
		return errors.New("max_concurrency must be 0 or more")
	}

	if cfg.CPUMillicores < 0 || cfg.CPUMillicores > maxCPUMillicores {
		return fmt.Errorf("cpu_millicores must be between 0 and %d", maxCPUMillicores)
	}

	if cfg.PidsMax < 0 || cfg.PidsMax > maxPidsMax {
		return fmt.Errorf("pids_max must be between 0 and %d", maxPidsMax)
	}

	if cfg.IOWeight < 0 || cfg.IOWeight > maxIOWeight {
		return fmt.Errorf("io_weight must be between 0 and %d", maxIOWeight)
	}

	return nil
}

// applyTenantDefaults fills the limits that differ between trusted and untrusted mode.
func applyTenantDefaults(cfg *appConfig, mode string) {
	if mode == tenantUntrusted {
		if cfg.MemoryMB == 0 {
			cfg.MemoryMB = untrustedDefaultMemoryMB
		}

		if cfg.DiskMB == 0 {
			cfg.DiskMB = untrustedDefaultDiskMB
		}

		if strings.TrimSpace(cfg.RequestTimeout) == "" {
			cfg.RequestTimeout = untrustedDefaultRequestTimeout
		}

		if cfg.Network == "" {
			cfg.Network = untrustedDefaultNetwork
		}
	}

	if cfg.CPUMillicores == 0 {
		cfg.CPUMillicores = defaultCPUMillicores
	}

	if cfg.PidsMax == 0 {
		cfg.PidsMax = defaultPidsMax
	}

	if cfg.IOWeight == 0 {
		cfg.IOWeight = defaultIOWeight
	}
}

// normaliseAppNetwork validates the network and identity modes for the tenant model.
func normaliseAppNetwork(cfg *appConfig, mode string) error {
	switch cfg.Network {
	case "", "inherit":
		cfg.Network = "inherit"
	case "none", "storage":
	default:
		return errors.New("network must be inherit, storage or none")
	}

	cfg.Identity = strings.ToLower(strings.TrimSpace(cfg.Identity))
	switch cfg.Identity {
	case "", identityModePlatform:
	default:
		return fmt.Errorf("identity must be empty or %s", identityModePlatform)
	}

	if mode == tenantUntrusted && cfg.Network == "inherit" {
		return errors.New("network inherit is not allowed in untrusted mode; use storage or none")
	}

	return nil
}

// applyAppTimeouts parses request, startup and health timings into their internal fields.
func applyAppTimeouts(cfg *appConfig) error {
	timeout, err := parseRequestTimeout(cfg.RequestTimeout)
	if err != nil {
		return err
	}

	cfg.requestTimeout = timeout
	if cfg.StartupTimeout == "" {
		cfg.StartupTimeout = defaultStartupTimeout.String()
	}

	startup, err := parseStartupTimeout(cfg.StartupTimeout)
	if err != nil {
		return err
	}

	cfg.startupTimeout = startup
	if cfg.HealthInterval == "" {
		cfg.HealthInterval = defaultHealthInterval.String()
	}

	interval, err := parseHealthInterval(cfg.HealthInterval)
	if err != nil {
		return err
	}

	cfg.healthInterval = interval

	failures, err := parseHealthFailures(cfg.HealthFailures)
	if err != nil {
		return err
	}

	cfg.HealthFailures = failures

	idle, err := parseIdleTimeout(cfg.IdleTimeout)
	if err != nil {
		return err
	}

	cfg.idleTimeout = idle

	value := strings.TrimSpace(cfg.IdleTimeout)
	switch {
	case len(cfg.Schedule) > 0 && value == "":
		// A scheduled app cannot sleep. Keep the field empty rather than storing an opt-out, so removing the schedule restores the default.
		cfg.idleTimeout = 0
		cfg.IdleTimeout = ""
	case idle == 0:
		// Store explicit opt-outs in one canonical spelling.
		cfg.IdleTimeout = "none"
	default:
		cfg.IdleTimeout = value
	}

	return nil
}

// validateAppSchedule checks each scheduled call's path and interval.
func validateAppSchedule(cfg *appConfig) error {
	if cfg.idleTimeout > 0 && len(cfg.Schedule) > 0 {
		return errors.New("idle_timeout cannot be used with schedule, because a stopped app cannot fire its timed calls")
	}

	for i := range cfg.Schedule {
		entry := &cfg.Schedule[i]
		if !strings.HasPrefix(entry.Path, "/") {
			return fmt.Errorf("schedule[%d]: path must start with /", i)
		}

		if _, err := parseScheduleInterval(*entry); err != nil {
			return fmt.Errorf("schedule[%d]: %w", i, err)
		}
	}

	return nil
}

// applyAppDefaults fills in tenant defaults, network and identity rules, timeouts and scheduled calls.
func applyAppDefaults(cfg *appConfig, mode string) error {
	applyTenantDefaults(cfg, mode)

	if err := normaliseAppNetwork(cfg, mode); err != nil {
		return err
	}

	if err := applyAppTimeouts(cfg); err != nil {
		return err
	}

	return validateAppSchedule(cfg)
}

// normaliseAppConfig fills in defaults and validates an app's runtime config. The optional mode applies untrusted defaults.
func normaliseAppConfig(cfg appConfig, name, domain, mode string) (appConfig, error) {
	cfg, err := normaliseAppAddress(cfg, name, domain)
	if err != nil {
		return cfg, err
	}

	cfg, err = normaliseAppRelations(cfg, name)
	if err != nil {
		return cfg, err
	}

	if err := validateAppSettings(&cfg); err != nil {
		return cfg, err
	}

	if err := applyAppDefaults(&cfg, mode); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// readAppConfig reads and normalises app.json, returning an empty config when the file is absent.
func readAppConfig(dir, name, domain, mode string) (appConfig, error) {
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

	return normaliseAppConfig(cfg, name, domain, mode)
}
