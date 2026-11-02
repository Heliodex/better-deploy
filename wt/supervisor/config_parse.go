package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// bindString reads the environment default and registers the flag against the same field.
func bindString(envName, flagName, usage string, target *string, fallback string) {
	*target = env(envName, fallback)
	flag.StringVar(target, flagName, *target, usage)
}

// bindBool reads the environment default and registers the flag against the same field.
func bindBool(envName, flagName, usage string, target *bool, fallback bool) {
	*target = envBool(envName, fallback)
	flag.BoolVar(target, flagName, *target, usage)
}

// bindInt reads the environment default and registers the flag against the same field.
func bindInt(envName, flagName, usage string, target *int, fallback int) {
	*target = int(envInt64(envName, int64(fallback)))
	flag.IntVar(target, flagName, *target, usage)
}

// bindInt64 reads the environment default and registers the flag against the same field.
func bindInt64(envName, flagName, usage string, target *int64, fallback int64) {
	*target = envInt64(envName, fallback)
	flag.Int64Var(target, flagName, *target, usage)
}

// bindConfigFlags loads every option from its environment variable and registers the matching flag in one place.
func bindConfigFlags(cfg *config, diskBudgetMB, storageTempBudgetMB *int64) {
	untrusted := strings.EqualFold(strings.TrimSpace(env("WT_TENANT_MODE", tenantTrusted)), tenantUntrusted)
	wasmtimeDefault := env("WT_WASMTIME", "wasmtime")

	bindString("WT_APPS_DIR", "apps", "directory containing one directory per app", &cfg.appsDir, "./apps")
	bindString("WT_STATE_DIR", "state", "per-app state directory", &cfg.stateDir, "./state")
	bindString("WT_CACHE_DIR", "cache-dir", "compiled cwasm cache (default WT_STATE_DIR/.cache)", &cfg.cacheDir, "")
	bindInt64("WT_CACHE_MAX_BYTES", "cache-max-bytes", "compiled cwasm cache byte cap", &cfg.cacheMaxBytes, defaultCacheMaxBytes)
	bindString("WT_LISTEN", "listen", "public listen address", &cfg.listen, "127.0.0.1:8080")
	cfg.storageListen = env("WT_STORAGE_LISTEN", "127.0.0.1:0")
	bindString("WT_APPS_DOMAIN", "domain", "default app host suffix", &cfg.domain, "localhost")
	bindString("WT_WASMTIME", "wasmtime", "wasmtime binary", &cfg.wasmtime, "wasmtime")
	bindString("WT_WASMTIME_COMPILER", "wasmtime-compiler", "wasmtime binary with the compile subcommand, used to rebuild stale artifacts", &cfg.wasmtimeCompiler, wasmtimeDefault)
	bindString("WT_WASMTIME_TARGET", "wasmtime-target", "wasmtime target triple (must match the compiler)", &cfg.wasmtimeTarget, "x86_64-unknown-linux-musl")
	bindString("WT_TOKEN", "token", "deploy bearer token (required off loopback and in untrusted mode)", &cfg.token, "")
	bindString("WT_TLS_CERT", "tls-cert", "TLS certificate (with --tls-key)", &cfg.tlsCert, "")
	bindString("WT_TLS_KEY", "tls-key", "TLS private key (with --tls-cert)", &cfg.tlsKey, "")
	bindString("WT_TENANT_MODE", "tenant-mode", "tenant trust model: trusted or untrusted", &cfg.tenantMode, tenantTrusted)
	bindString("WT_ADMIN_LISTEN", "admin-listen", "separate admin listener (TCP address or unix:/path)", &cfg.adminListen, "")
	bindString("WT_APP_FETCH_LISTEN", "app-fetch-listen", "internal app-to-app fetch listener (loopback TCP address)", &cfg.appFetchListen, "127.0.0.1:0")
	bindString("WT_AUTH_FILE", "auth-file", "scoped token store (default WT_STATE_DIR/.auth/tokens.json)", &cfg.authFile, "")
	bindString("WT_TRUSTED_PROXIES", "trusted-proxies", "comma-separated proxy IPs or CIDRs whose X-Forwarded-* headers are trusted", &cfg.trustedProxiesValue, "")
	bindString("WT_CGROUP_ROOT", "cgroup-root", "cgroup v2 root for per-app limits", &cfg.cgroupRoot, "/sys/fs/cgroup/wt")
	bindString("WT_IPTABLES", "iptables", "iptables binary for per-app IPv4 egress rules", &cfg.iptables, "iptables")
	bindString("WT_IP6TABLES", "ip6tables", "ip6tables binary for per-app IPv6 egress rules", &cfg.ip6tables, "ip6tables")
	bindString("WT_DATA_QUOTA_MODE", "data-quota-mode", "untrusted /data quota backend: monitor or setquota", &cfg.dataQuotaMode, dataQuotaMonitor)
	bindString("WT_SETQUOTA", "setquota", "setquota binary for per-uid filesystem quotas", &cfg.setquota, "setquota")
	bindString("WT_FINDMNT", "findmnt", "findmnt binary used to resolve the quota filesystem", &cfg.findmnt, "findmnt")
	bindString("WT_DATA_QUOTA_FS", "data-quota-fs", "filesystem or mount point for per-uid quotas (default: resolve WT_STATE_DIR with findmnt)", &cfg.dataQuotaFS, "")
	bindInt("WT_APP_UID_BASE", "app-uid-base", "first allocated uid/gid for apps in untrusted mode", &cfg.appUIDBase, 20000)
	bindBool("WT_IPV6_EGRESS", "ipv6-egress", "reject IPv6 egress from untrusted app uids", &cfg.ipv6Egress, true)
	bindBool("WT_APP_NAMESPACES", "app-namespaces", "run apps in user, PID, mount, IPC and UTS namespaces (default on in untrusted mode)", &cfg.appNamespaces, untrusted)
	bindBool("WT_APP_SECCOMP", "app-seccomp", "install the app seccomp filter (default on in untrusted mode)", &cfg.appSeccomp, untrusted)
	bindBool("WT_ACCESS_LOG", "access-log", "write proxied request lines to each app's supervisor log", &cfg.accessLog, true)
	bindInt("WT_ACCESS_LOG_EVERY", "access-log-every", "log one in N successful access lines (errors are always logged)", &cfg.accessLogEvery, 1)
	bindInt64("WT_DISK_BUDGET_MB", "disk-budget-mb", "global disk budget in MiB across apps and state (0 = unlimited)", diskBudgetMB, 0)
	cfg.storageMigrationTimeout = envStorageMigrationTimeout()
	flag.DurationVar(&cfg.storageMigrationTimeout, "storage-migration-timeout", cfg.storageMigrationTimeout, "timeout for one migration, between 1s and 1h (default 5m)")
	bindString("WT_STORAGE_TMP_DIR", "storage-tmp-dir", "directory for SQLite temp files (default WT_STATE_DIR/.tmp)", &cfg.storageTmpDir, "")
	bindInt64("WT_STORAGE_TEMP_BUDGET_MB", "storage-temp-budget-mb", "global SQLite temp directory budget in MiB (0 = unlimited)", storageTempBudgetMB, defaultStorageTempBudgetMB)
}

// warnConfig logs the trust-model warnings that do not stop startup.
func warnConfig(cfg config) {
	if cfg.untrusted() {
		log.Printf("untrusted tenant mode: per-app uid, cgroup and network isolation are required (namespaces=%t seccomp=%t)", cfg.appNamespaces, cfg.appSeccomp)
		if os.Geteuid() != 0 {
			log.Printf("warning: untrusted tenant mode needs root; apps will refuse to launch")
		}
		if cfg.adminListen == "" {
			log.Printf("warning: WT_ADMIN_LISTEN is unset; admin routes stay on the public listener behind WT_TOKEN")
		}
		if !cfg.ipv6Egress {
			log.Printf("warning: WT_IPV6_EGRESS is off; untrusted apps have no IPv6 egress boundary")
		}
		return
	}
	if cfg.token == "" && !loopbackListen(cfg.listen) {
		log.Fatalf("WT_TOKEN is required when listening on %s", cfg.listen)
	}
	if cfg.token == "" {
		log.Printf("warning: WT_TOKEN is empty; admin and deploy APIs are unauthenticated on the loopback listener")
	}
}

// applyConfigFlags validates the parsed flags and fills in the derived limits and trust settings.
func applyConfigFlags(cfg *config, diskBudgetMB, storageTempBudgetMB int64) {
	if cfg.storageMigrationTimeout < minStorageMigrationTimeout || cfg.storageMigrationTimeout > maxStorageMigrationTimeout {
		log.Fatalf("storage migration timeout %s must be between %s and %s", cfg.storageMigrationTimeout, minStorageMigrationTimeout, maxStorageMigrationTimeout)
	}
	if storageTempBudgetMB < 0 {
		log.Fatalf("storage temp budget must be 0 or greater")
	}
	cfg.storageTempBudgetBytes = storageTempBudgetMB << 20
	if cfg.storageTempBudgetBytes > 1<<40 {
		log.Fatalf("storage temp budget must be at most 1 TiB")
	}
	if diskBudgetMB > 0 {
		cfg.diskBudgetBytes = diskBudgetMB << 20
	} else {
		cfg.diskBudgetBytes = 0
	}
	cfg.tenantMode = strings.ToLower(strings.TrimSpace(cfg.tenantMode))
	if err := validateTenantConfig(*cfg); err != nil {
		log.Fatalf("%v", err)
	}
	if err := validateIsolationConfig(*cfg); err != nil {
		log.Fatalf("%v", err)
	}
	if cfg.appNamespaces && !cfg.untrusted() && os.Geteuid() == 0 {
		log.Printf("warning: app namespaces in trusted mode need a non-root supervisor; launches will fail")
	}
	if err := validateAppFetchListen(cfg.appFetchListen); err != nil {
		log.Fatalf("%v", err)
	}
	if cfg.accessLogEvery < 1 || cfg.accessLogEvery > 10000 {
		log.Fatalf("access-log-every must be between 1 and 10000")
	}
	trustedProxies, err := parseTrustedProxies(cfg.trustedProxiesValue)
	if err != nil {
		log.Fatalf("%v", err)
	}
	cfg.trustedProxies = trustedProxies
	if err := validateDataQuotaConfig(cfg); err != nil {
		log.Fatalf("%v", err)
	}
	if !cfg.untrusted() && cfg.dataQuotaMode == dataQuotaSetquota {
		log.Printf("warning: WT_DATA_QUOTA_MODE=setquota is ignored in trusted mode; disk_mb stays a report")
	}
	warnConfig(*cfg)
}

// resolveConfigPaths turns the configured directories into absolute paths and creates them.
func resolveConfigPaths(cfg *config) {
	absoluteApps, err := filepath.Abs(cfg.appsDir)
	if err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	absoluteState, err := filepath.Abs(cfg.stateDir)
	if err != nil {
		log.Fatalf("state dir: %v", err)
	}
	cfg.appsDir, cfg.stateDir = absoluteApps, absoluteState
	if cfg.storageTmpDir == "" {
		cfg.storageTmpDir = filepath.Join(cfg.stateDir, ".tmp")
	} else {
		absoluteTmp, err := filepath.Abs(cfg.storageTmpDir)
		if err != nil {
			log.Fatalf("storage temp dir: %v", err)
		}
		cfg.storageTmpDir = absoluteTmp
	}
	if err := validateStorageTempDir(cfg.storageTmpDir, cfg.stateDir, cfg.appsDir); err != nil {
		log.Fatalf("%v", err)
	}
	if cfg.cacheDir == "" {
		cfg.cacheDir = filepath.Join(cfg.stateDir, ".cache")
	} else {
		absoluteCache, err := filepath.Abs(cfg.cacheDir)
		if err != nil {
			log.Fatalf("cache dir: %v", err)
		}
		cfg.cacheDir = absoluteCache
	}
	if cfg.cacheMaxBytes <= 0 {
		cfg.cacheMaxBytes = defaultCacheMaxBytes
	}
	if err := os.MkdirAll(cfg.appsDir, 0o755); err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	if err := os.MkdirAll(cfg.cacheDir, 0o755); err != nil {
		log.Fatalf("cache dir: %v", err)
	}
}

// parseConfig reads flags and environment, then prepares the runtime directories.
func parseConfig() config {
	cfg := config{}
	diskBudgetMB := int64(0)
	storageTempBudgetMB := int64(defaultStorageTempBudgetMB)
	bindConfigFlags(&cfg, &diskBudgetMB, &storageTempBudgetMB)
	flag.Parse()
	applyConfigFlags(&cfg, diskBudgetMB, storageTempBudgetMB)
	resolveConfigPaths(&cfg)
	return cfg
}
