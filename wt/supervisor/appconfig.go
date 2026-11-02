// App configuration: environment, flags and app.json validation.

package main

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStartupTimeout = 15 * time.Second
	minStartupTimeout     = time.Second
	maxStartupTimeout     = 10 * time.Minute

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
		return fmt.Errorf("storage temp dir must not be the filesystem root")
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

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envBool reads a boolean opt-in/opt-out. An unknown value keeps the fallback, so a typo cannot silently disable a boundary.
func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return fallback
	}
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

// envStorageMigrationTimeout applies WT_STORAGE_MIGRATION_TIMEOUT, falling back to the default for an empty or invalid value. The explicit flag is validated separately.
func envStorageMigrationTimeout() time.Duration {
	timeout, err := parseStorageMigrationTimeout(os.Getenv("WT_STORAGE_MIGRATION_TIMEOUT"))
	if err != nil {
		return defaultStorageMigrationTimeout
	}
	return timeout
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

// normalizeAllowCallers validates the target app's inbound app-to-app allowlist. Empty entries are ignored so a panel comma-separated field stays convenient.
func normalizeAllowCallers(names []string) ([]string, error) {
	if len(names) > maxAllowCallers {
		return nil, fmt.Errorf("allow_callers must contain at most %d apps", maxAllowCallers)
	}
	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if !validName(name) {
			return nil, fmt.Errorf("allow_callers: %q is not a valid app name", name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// normalizeDependsOn validates the apps this app waits for before its host routes traffic. Forward references are allowed because a dependency may be deployed later.
func normalizeDependsOn(names []string, self string) ([]string, error) {
	if len(names) > maxDependsOn {
		return nil, fmt.Errorf("depends_on must contain at most %d apps", maxDependsOn)
	}
	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if name == self {
			return nil, fmt.Errorf("depends_on must not include the app itself")
		}
		if !validName(name) {
			return nil, fmt.Errorf("depends_on: %q is not a valid app name", name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// normalizeEgress validates an explicit untrusted-mode outbound allowlist. CIDR rules are IPv4 only because the IPv6 chain remains deny-all.
func normalizeEgress(rules []egressRule) ([]egressRule, error) {
	if len(rules) > maxEgressRules {
		return nil, fmt.Errorf("egress must contain at most %d rules", maxEgressRules)
	}
	out := make([]egressRule, 0, len(rules))
	seen := map[egressRule]struct{}{}
	for _, rule := range rules {
		_, network, err := net.ParseCIDR(strings.TrimSpace(rule.CIDR))
		if err != nil || network.IP.To4() == nil {
			return nil, fmt.Errorf("egress %q: use an IPv4 CIDR such as 203.0.113.0/24", rule.CIDR)
		}
		if rule.Port < 1 || rule.Port > 65535 {
			return nil, fmt.Errorf("egress %q: port must be between 1 and 65535", network)
		}
		next := egressRule{CIDR: network.String(), Port: rule.Port}
		if _, ok := seen[next]; ok {
			continue
		}
		seen[next] = struct{}{}
		out = append(out, next)
	}
	slices.SortFunc(out, func(a, b egressRule) int {
		if a.CIDR != b.CIDR {
			return strings.Compare(a.CIDR, b.CIDR)
		}
		return a.Port - b.Port
	})
	return out, nil
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

func defaultHost(name, domain string) string {
	if domain != "" {
		return name + "." + domain
	}
	return name + ".localhost"
}

// parseRequestTimeout validates an optional per-request deadline. Empty means no configured deadline;
// "0" and "none" are explicit ways to disable it, which matters where untrusted mode supplies a default.
func parseRequestTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" || strings.EqualFold(value, "none") {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < time.Second || timeout > 24*time.Hour {
		return 0, fmt.Errorf("request_timeout %q must be a duration between 1s and 24h, or 0/none to disable", value)
	}
	return timeout, nil
}

// parseStartupTimeout validates how long a new process may take to answer its health path.
func parseStartupTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return defaultStartupTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minStartupTimeout || timeout > maxStartupTimeout {
		return 0, fmt.Errorf("startup_timeout %q must be a duration between %s and %s", value, minStartupTimeout, maxStartupTimeout)
	}
	return timeout, nil
}

// parseHealthInterval validates how often a running app is probed.
func parseHealthInterval(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return defaultHealthInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < minHealthInterval || interval > maxHealthInterval {
		return 0, fmt.Errorf("health_interval %q must be a duration between %s and %s", value, minHealthInterval, maxHealthInterval)
	}
	return interval, nil
}

// parseHealthFailures validates the consecutive failure count that marks an app unready.
func parseHealthFailures(value int) (int, error) {
	if value == 0 {
		return defaultHealthFailures, nil
	}
	if value < minHealthFailures || value > maxHealthFailures {
		return 0, fmt.Errorf("health_failures must be between %d and %d", minHealthFailures, maxHealthFailures)
	}
	return value, nil
}

func (cfg appConfig) startupLimit() time.Duration {
	if cfg.startupTimeout > 0 {
		return cfg.startupTimeout
	}
	return defaultStartupTimeout
}

func (cfg appConfig) probeInterval() time.Duration {
	if cfg.healthInterval > 0 {
		return cfg.healthInterval
	}
	return defaultHealthInterval
}

func (cfg appConfig) failureLimit() int {
	if cfg.HealthFailures > 0 {
		return cfg.HealthFailures
	}
	return defaultHealthFailures
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

// normalizeAppAddress fills in and validates the app name, host and host aliases.
func normalizeAppAddress(cfg appConfig, name, domain string) (appConfig, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = name
	}
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = defaultHost(name, domain)
	}
	cfg.Host = strings.ToLower(strings.TrimSpace(cfg.Host))
	if err := validateAppHost(cfg.Host, domain); err != nil {
		return cfg, err
	}
	hosts, err := normalizeAppHosts(cfg.Hosts, cfg.Host, domain)
	if err != nil {
		return cfg, err
	}
	cfg.Hosts = hosts
	return cfg, nil
}

// normalizeAppRelations validates the caller allowlist, dependencies and egress rules.
func normalizeAppRelations(cfg appConfig, name string) (appConfig, error) {
	callers, err := normalizeAllowCallers(cfg.AllowCallers)
	if err != nil {
		return cfg, err
	}
	cfg.AllowCallers = callers
	dependsOn, err := normalizeDependsOn(cfg.DependsOn, name)
	if err != nil {
		return cfg, err
	}
	cfg.DependsOn = dependsOn
	egress, err := normalizeEgress(cfg.Egress)
	if err != nil {
		return cfg, err
	}
	cfg.Egress = egress
	return cfg, nil
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
		return fmt.Errorf("health must start with /")
	}
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
		return fmt.Errorf("max_concurrency must be 0 or more")
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

// normalizeAppNetwork validates the network and identity modes for the tenant model.
func normalizeAppNetwork(cfg *appConfig, mode string) error {
	switch cfg.Network {
	case "", "inherit":
		cfg.Network = "inherit"
	case "none", "storage":
	default:
		return fmt.Errorf("network must be inherit, storage or none")
	}
	cfg.Identity = strings.ToLower(strings.TrimSpace(cfg.Identity))
	switch cfg.Identity {
	case "", identityModePlatform:
	default:
		return fmt.Errorf("identity must be empty or %s", identityModePlatform)
	}
	if mode == tenantUntrusted && cfg.Network == "inherit" {
		return fmt.Errorf("network inherit is not allowed in untrusted mode; use storage or none")
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
	return nil
}

// validateAppSchedule checks each scheduled call's path and interval.
func validateAppSchedule(cfg *appConfig) error {
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
	if err := normalizeAppNetwork(cfg, mode); err != nil {
		return err
	}
	if err := applyAppTimeouts(cfg); err != nil {
		return err
	}
	return validateAppSchedule(cfg)
}

// normalizeAppConfig fills in defaults and validates an app's runtime config. The optional mode applies untrusted defaults.
func normalizeAppConfig(cfg appConfig, name, domain, mode string) (appConfig, error) {
	cfg, err := normalizeAppAddress(cfg, name, domain)
	if err != nil {
		return cfg, err
	}
	cfg, err = normalizeAppRelations(cfg, name)
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
	return normalizeAppConfig(cfg, name, domain, mode)
}
