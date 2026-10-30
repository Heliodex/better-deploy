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
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	wasmEpoch      bool
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
	if err := recoverRollbacks(cfg.stateDir, s.backups); err != nil {
		log.Fatalf("rollback recovery: %v", err)
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
