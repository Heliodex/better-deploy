package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// bindConfigFlags registers every supervisor flag against the environment-derived defaults.
func bindConfigFlags(cfg *config, diskBudgetMB, storageTempBudgetMB *int64) {
	flag.StringVar(&cfg.listen, "listen", cfg.listen, "public listen address")
	flag.StringVar(&cfg.appsDir, "apps", cfg.appsDir, "directory containing one directory per app")
	flag.StringVar(&cfg.stateDir, "state", cfg.stateDir, "per-app state directory")
	flag.StringVar(&cfg.cacheDir, "cache-dir", cfg.cacheDir, "compiled cwasm cache (default WT_STATE_DIR/.cache)")
	flag.Int64Var(&cfg.cacheMaxBytes, "cache-max-bytes", cfg.cacheMaxBytes, "compiled cwasm cache byte cap")
	flag.Int64Var(diskBudgetMB, "disk-budget-mb", *diskBudgetMB, "global disk budget in MiB across apps and state (0 = unlimited)")
	flag.BoolVar(&cfg.accessLog, "access-log", cfg.accessLog, "write proxied request lines to each app's supervisor log")
	flag.IntVar(&cfg.accessLogEvery, "access-log-every", cfg.accessLogEvery, "log one in N successful access lines (errors are always logged)")
	flag.DurationVar(&cfg.storageMigrationTimeout, "storage-migration-timeout", cfg.storageMigrationTimeout, "timeout for one migration, between 1s and 1h (default 5m)")
	flag.StringVar(&cfg.storageTmpDir, "storage-tmp-dir", cfg.storageTmpDir, "directory for SQLite temp files (default WT_STATE_DIR/.tmp)")
	flag.Int64Var(storageTempBudgetMB, "storage-temp-budget-mb", *storageTempBudgetMB, "global SQLite temp directory budget in MiB (0 = unlimited)")
	flag.StringVar(&cfg.domain, "domain", cfg.domain, "default app host suffix")
	flag.StringVar(&cfg.wasmtime, "wasmtime", cfg.wasmtime, "wasmtime binary")
	flag.StringVar(&cfg.wasmtimeCompiler, "wasmtime-compiler", cfg.wasmtimeCompiler, "wasmtime binary with the compile subcommand, used to rebuild stale artifacts")
	flag.StringVar(&cfg.wasmtimeTarget, "wasmtime-target", cfg.wasmtimeTarget, "wasmtime target triple (must match the compiler)")
	flag.StringVar(&cfg.token, "token", cfg.token, "deploy bearer token (required off loopback and in untrusted mode)")
	flag.StringVar(&cfg.tlsCert, "tls-cert", cfg.tlsCert, "TLS certificate (with --tls-key)")
	flag.StringVar(&cfg.tlsKey, "tls-key", cfg.tlsKey, "TLS private key (with --tls-cert)")
	flag.StringVar(&cfg.tenantMode, "tenant-mode", cfg.tenantMode, "tenant trust model: trusted or untrusted")
	flag.StringVar(&cfg.adminListen, "admin-listen", cfg.adminListen, "separate admin listener (TCP address or unix:/path)")
	flag.StringVar(&cfg.appFetchListen, "app-fetch-listen", cfg.appFetchListen, "internal app-to-app fetch listener (loopback TCP address)")
	flag.StringVar(&cfg.authFile, "auth-file", cfg.authFile, "scoped token store (default WT_STATE_DIR/.auth/tokens.json)")
	flag.StringVar(&cfg.trustedProxiesValue, "trusted-proxies", cfg.trustedProxiesValue, "comma-separated proxy IPs or CIDRs whose X-Forwarded-* headers are trusted")
	flag.StringVar(&cfg.cgroupRoot, "cgroup-root", cfg.cgroupRoot, "cgroup v2 root for per-app limits")
	flag.StringVar(&cfg.iptables, "iptables", cfg.iptables, "iptables binary for per-app IPv4 egress rules")
	flag.StringVar(&cfg.ip6tables, "ip6tables", cfg.ip6tables, "ip6tables binary for per-app IPv6 egress rules")
	flag.StringVar(&cfg.dataQuotaMode, "data-quota-mode", cfg.dataQuotaMode, "untrusted /data quota backend: monitor or setquota")
	flag.StringVar(&cfg.setquota, "setquota", cfg.setquota, "setquota binary for per-uid filesystem quotas")
	flag.StringVar(&cfg.findmnt, "findmnt", cfg.findmnt, "findmnt binary used to resolve the quota filesystem")
	flag.StringVar(&cfg.dataQuotaFS, "data-quota-fs", cfg.dataQuotaFS, "filesystem or mount point for per-uid quotas (default: resolve WT_STATE_DIR with findmnt)")
	flag.BoolVar(&cfg.ipv6Egress, "ipv6-egress", cfg.ipv6Egress, "reject IPv6 egress from untrusted app uids")
	flag.IntVar(&cfg.appUIDBase, "app-uid-base", cfg.appUIDBase, "first allocated uid/gid for apps in untrusted mode")
	flag.BoolVar(&cfg.appNamespaces, "app-namespaces", cfg.appNamespaces, "run apps in user, PID, mount, IPC and UTS namespaces (default on in untrusted mode)")
	flag.BoolVar(&cfg.appSeccomp, "app-seccomp", cfg.appSeccomp, "install the app seccomp filter (default on in untrusted mode)")
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

func parseConfig() config {
	cfg := configFromEnv()
	diskBudgetMB := cfg.diskBudgetBytes >> 20
	storageTempBudgetMB := cfg.storageTempBudgetBytes >> 20
	bindConfigFlags(&cfg, &diskBudgetMB, &storageTempBudgetMB)
	flag.Parse()
	applyConfigFlags(&cfg, diskBudgetMB, storageTempBudgetMB)
	resolveConfigPaths(&cfg)
	return cfg
}
