// Command wt-supervisor runs WASI HTTP components with wasmtime and routes requests by host.
//
// Each app lives in its own directory under WT_APPS_DIR with an app.json and a precompiled main.cwasm, plus main.wasm and artifact.json so an upgraded runtime can rebuild it. The supervisor starts one `wasmtime serve` process per app on a loopback port, proxies by Host, and deploys new bundles blue/green.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxUploadBytes  = 256 << 20
	maxExtractBytes = 512 << 20
	maxTarFiles     = 10_000
	// Backup archives are internal and may hold more data than an uploaded bundle.
	maxBackupExtractBytes = 8 << 30
	maxBackupTarFiles     = 100_000
	readyTimeout          = 15 * time.Second
	stopTimeout           = 5 * time.Second
	drainTimeout          = 10 * time.Second
)

type config struct {
	appsDir, stateDir, listen, storageListen, domain string
	wasmtime, wasmtimeCompiler, wasmtimeTarget       string
	token, tlsCert, tlsKey                           string
}

type appConfig struct {
	Name           string            `json:"name"`
	Host           string            `json:"host"`
	Health         string            `json:"health"`
	Env            map[string]string `json:"env"`
	Build          string            `json:"build,omitempty"`
	MemoryMB       int               `json:"memory_mb,omitempty"`
	DiskMB         int               `json:"disk_mb,omitempty"`
	MaxConcurrent  int               `json:"max_concurrency,omitempty"`
	RequestTimeout string            `json:"request_timeout,omitempty"`
	Network        string            `json:"network,omitempty"`
	Schedule       []scheduleConfig  `json:"schedule,omitempty"`

	requestTimeout time.Duration
}

// scheduleConfig asks the supervisor to call an app endpoint on a timer. Components cannot run their own background loops under `wasmtime serve`, so time is driven from the host.
type scheduleConfig struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	Every  string `json:"every"`
}

const (
	defaultScheduleMethod = http.MethodPost
	minScheduleInterval   = time.Second
	maxScheduleInterval   = 24 * time.Hour
)

type app struct {
	name, dir string
	port      int
	cfg       appConfig
	proxy     *httputil.ReverseProxy
	started   time.Time
	delay     time.Duration
	timer     *time.Timer
	logs      *appLog
	stats     appStats
	disk      diskUsageCache
	history   diskUsageCache
	requests  sync.WaitGroup // in-flight proxied requests, drained before a replaced version stops

	mu              sync.Mutex
	scheduleCancel  context.CancelFunc
	cmd             *exec.Cmd
	done            chan struct{}
	exited, retired bool
	starting        bool

	slots          chan struct{}
	healthOK       bool
	healthFailures int
	lastHealthy    time.Time
	lastError      string
}

type supervisor struct {
	cfg         config
	storage     *storageServer
	backups     *backupManager
	events      *hub
	deployments *deploymentManager

	mu          sync.RWMutex
	apps, hosts map[string]*app

	deployMu sync.Mutex
	stopping atomic.Bool

	versionOnce sync.Once
	version     string
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func configFromEnv() config {
	return config{
		appsDir:          env("WT_APPS_DIR", "./apps"),
		stateDir:         env("WT_STATE_DIR", "./state"),
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

func freePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func minimalEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}
	env := make([]string, 0, len(keep))
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// maxLaunchLogBytes caps how much of a wasmtime process's output is kept for failed-launch errors.
const maxLaunchLogBytes = 8 << 10

// logRing keeps the tail of a process's output for failed-launch errors, so deploys report why wasmtime exited.
type logRing struct {
	mu  sync.Mutex
	buf []byte
}

func newLogRing() *logRing {
	return &logRing{}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > maxLaunchLogBytes {
		r.buf = append([]byte{}, r.buf[len(r.buf)-maxLaunchLogBytes:]...)
	}
	return len(p), nil
}

func (r *logRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}

// withLaunchLog appends the tail of the failed process output to a launch error.
func withLaunchLog(source string, recent *logRing, err error) error {
	tail := recent.String()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\n--- %s output ---\n%s", err, filepath.Base(source), tail)
}

func (s *supervisor) hostTaken(host, except string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, a := range s.apps {
		if name != except && a.cfg.Host == host {
			return true
		}
	}
	return false
}

func (s *supervisor) authorized(r *http.Request) bool {
	if s.cfg.token == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if token, ok := strings.CutPrefix(header, prefix); ok {
		got := strings.TrimSpace(token)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.token)) == 1 {
			return true
		}
	}
	cookie, err := r.Cookie("wt_token")
	if err != nil {
		return false
	}
	got := strings.TrimSpace(cookie.Value)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := json.Marshal(value)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// requireMethod writes 405 with an Allow header unless the request uses method.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (s *supervisor) info(a *app) appInfo {
	info := appInfo{Name: a.name, Port: a.port}
	a.mu.Lock()
	info.Host = a.cfg.Host
	info.StartedAt = a.started
	info.DiskLimitBytes = int64(a.cfg.DiskMB) << 20
	info.Status = a.statusLocked()
	info.LastHealthy = a.lastHealthy
	info.LastError = a.lastError
	if a.cmd != nil && a.cmd.Process != nil && !a.exited {
		info.PID = a.cmd.Process.Pid
	}
	a.mu.Unlock()
	info.DiskBytes = s.diskBytes(a)
	info.HistoryBytes = s.historyBytes(a)
	info.OverQuota = info.DiskLimitBytes > 0 && info.DiskBytes > info.DiskLimitBytes
	info.CPUPercent, info.MemoryBytes, info.SampledAt = a.resourceStats()
	return info
}

func (s *supervisor) handleApps(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

// lookup returns the app with the given name, or nil.
func (s *supervisor) lookup(name string) *app {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apps[name]
}

// allApps returns a snapshot of the registered apps in an unspecified order.
func (s *supervisor) allApps() []*app {
	s.mu.RLock()
	defer s.mu.RUnlock()
	apps := make([]*app, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	return apps
}

type appInfo struct {
	Name           string    `json:"name"`
	Host           string    `json:"host"`
	Port           int       `json:"port"`
	Status         string    `json:"status"`
	PID            int       `json:"pid,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	CPUPercent     float64   `json:"cpuPercent"`
	MemoryBytes    int64     `json:"memoryBytes"`
	DiskBytes      int64     `json:"diskBytes"`
	DiskLimitBytes int64     `json:"diskLimitBytes,omitempty"`
	HistoryBytes   int64     `json:"historyBytes"`
	OverQuota      bool      `json:"overQuota,omitempty"`
	SampledAt      time.Time `json:"sampledAt"`
	LastHealthy    time.Time `json:"lastHealthy,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
}

func (s *supervisor) stopAll() {
	apps := s.allApps()

	var wg sync.WaitGroup
	for _, a := range apps {
		wg.Add(1)
		go func(a *app) {
			defer wg.Done()
			s.stop(a)
		}(a)
	}
	wg.Wait()
}

func (s *supervisor) handleApp(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/")
	name, action, _ := strings.Cut(rest, "/")
	if name == "" || !validName(name) {
		http.Error(w, "invalid app name", http.StatusBadRequest)
		return
	}

	if action == "backups" || strings.HasPrefix(action, "backups/") {
		s.handleBackupRoute(w, r, name, strings.TrimPrefix(action, "backups"))
		return
	}
	if id, ok := strings.CutPrefix(action, "rollback/"); ok {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleRollback(w, r, name, id)
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			a, ok := s.requireApp(w, r, name)
			if !ok {
				return
			}
			a.sampleResources(time.Now())
			writeJSON(w, http.StatusOK, s.info(a))
		case http.MethodPost:
			if !s.requireAuth(w, r) {
				return
			}
			s.deploy(w, r, name)
		case http.MethodDelete:
			s.handleDelete(w, r, name)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "config":
		s.handleAppConfig(w, r, name)
	case "deployments":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleDeployments(w, r, name)
	case "stop":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleStop(w, r, name)
	case "start":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleStart(w, r, name)
	case "logs/stream":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		a, ok := s.requireApp(w, r, name)
		if !ok {
			return
		}
		s.handleLogStream(w, r, a)
	case "logs":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		a, ok := s.requireApp(w, r, name)
		if !ok {
			return
		}
		s.handleLogs(w, r, a)
	case "resources":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		a, ok := s.requireApp(w, r, name)
		if !ok {
			return
		}
		s.handleResources(w, r, a)
	case "database":
		s.handleDatabase(w, r, name)
	case "database/rows":
		s.handleDatabaseRows(w, r, name)
	case "database/changes":
		s.handleDatabaseChanges(w, r, name)
	case "database/export":
		s.handleDatabaseExport(w, r, name)
	case "database/import":
		s.handleDatabaseImport(w, r, name)
	case "database/maintenance":
		s.handleDatabaseMaintenance(w, r, name)
	case "restart":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		a, ok := s.requireApp(w, r, name)
		if !ok {
			return
		}
		s.handleRestart(w, r, a)
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

// loopbackListen reports whether addr only accepts loopback connections.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := configFromEnv()
	flag.StringVar(&cfg.listen, "listen", cfg.listen, "public listen address")
	flag.StringVar(&cfg.appsDir, "apps", cfg.appsDir, "directory containing one directory per app")
	flag.StringVar(&cfg.stateDir, "state", cfg.stateDir, "per-app state directory")
	flag.StringVar(&cfg.domain, "domain", cfg.domain, "default app host suffix")
	flag.StringVar(&cfg.wasmtime, "wasmtime", cfg.wasmtime, "wasmtime binary")
	flag.StringVar(&cfg.wasmtimeCompiler, "wasmtime-compiler", cfg.wasmtimeCompiler, "wasmtime binary with the compile subcommand, used to rebuild stale artifacts")
	flag.StringVar(&cfg.wasmtimeTarget, "wasmtime-target", cfg.wasmtimeTarget, "wasmtime target triple (must match the compiler)")
	flag.StringVar(&cfg.token, "token", cfg.token, "deploy bearer token (required off loopback)")
	flag.StringVar(&cfg.tlsCert, "tls-cert", cfg.tlsCert, "TLS certificate (with --tls-key)")
	flag.StringVar(&cfg.tlsKey, "tls-key", cfg.tlsKey, "TLS private key (with --tls-cert)")
	flag.Parse()

	if cfg.token == "" && !loopbackListen(cfg.listen) {
		log.Fatalf("WT_TOKEN is required when listening on %s", cfg.listen)
	}
	if cfg.token == "" {
		log.Printf("warning: WT_TOKEN is empty; admin and deploy APIs are unauthenticated on the loopback listener")
	}

	absoluteApps, err := filepath.Abs(cfg.appsDir)
	if err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	absoluteState, err := filepath.Abs(cfg.stateDir)
	if err != nil {
		log.Fatalf("state dir: %v", err)
	}
	cfg.appsDir, cfg.stateDir = absoluteApps, absoluteState
	if err := os.MkdirAll(cfg.appsDir, 0o755); err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}

	storage, err := newStorageServer(filepath.Join(cfg.stateDir, ".storage"), cfg.storageListen)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	s := &supervisor{
		cfg:         cfg,
		storage:     storage,
		backups:     newBackupManager(filepath.Join(cfg.stateDir, ".backups"), storage, cfg.stateDir),
		events:      newHub(),
		deployments: newDeploymentManager(filepath.Join(cfg.stateDir, ".deployments")),
		apps:        map[string]*app{},
		hosts:       map[string]*app{},
	}
	if err := s.loadApps(); err != nil {
		log.Fatalf("load apps: %v", err)
	}
	backupCtx, stopBackups := context.WithCancel(context.Background())
	defer stopBackups()
	go s.backups.loop(backupCtx)
	go s.monitorResources(resourceSampleInterval)
	go s.monitorHealth()

	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		s.stopping.Store(true)
		log.Printf("shutting down")
		_ = server.Close()
	}()

	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatalf("--tls-cert and --tls-key must be set together")
	}
	log.Printf("supervisor listening on %s with %d app(s)", cfg.listen, len(s.apps))
	if cfg.tlsCert != "" {
		err = server.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	s.stopAll()
	s.persistResources()
	storage.close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
