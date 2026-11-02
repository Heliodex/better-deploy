// Command wt-supervisor runs WASI HTTP components with wasmtime and routes requests by host.
//
// Each app lives in its own directory under WT_APPS_DIR with an app.json and a precompiled main.cwasm, plus main.wasm and artifact.json so an upgraded runtime can rebuild it. The supervisor starts one `wasmtime serve` process per app on a loopback port, proxies by Host, and deploys new bundles blue/green.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
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
	stopTimeout           = 5 * time.Second
	drainTimeout          = 10 * time.Second
	shutdownTimeout       = 15 * time.Second
	defaultCacheMaxBytes  = 2 << 30
)

type config struct {
	appsDir, stateDir, cacheDir, listen, storageListen, domain string
	wasmtime, wasmtimeCompiler, wasmtimeTarget                 string
	token, tlsCert, tlsKey                                     string
	tenantMode, adminListen, appFetchListen                    string
	authFile                                                   string
	trustedProxiesValue                                        string
	trustedProxies                                             []netip.Prefix
	cgroupRoot, iptables, ip6tables                            string
	dataQuotaMode, setquota, findmnt, dataQuotaFS              string
	appUIDBase                                                 int
	ipv6Egress                                                 bool
	appNamespaces, appSeccomp                                  bool
	cacheMaxBytes                                              int64
	diskBudgetBytes                                            int64
	accessLog                                                  bool
	accessLogEvery                                             int
	storageMigrationTimeout                                    time.Duration
	storageTmpDir                                              string
	storageTempBudgetBytes                                     int64
}

type appConfig struct {
	Name               string            `json:"name"`
	Host               string            `json:"host"`
	Hosts              []string          `json:"hosts,omitempty"`
	Health             string            `json:"health"`
	Env                map[string]string `json:"env"`
	Build              string            `json:"build,omitempty"`
	MemoryMB           int               `json:"memory_mb,omitempty"`
	DiskMB             int               `json:"disk_mb,omitempty"`
	CPUMillicores      int               `json:"cpu_millicores,omitempty"`
	PidsMax            int               `json:"pids_max,omitempty"`
	IOWeight           int               `json:"io_weight,omitempty"`
	MaxConcurrent      int               `json:"max_concurrency,omitempty"`
	RequestTimeout     string            `json:"request_timeout,omitempty"`
	StartupTimeout     string            `json:"startup_timeout,omitempty"`
	HealthInterval     string            `json:"health_interval,omitempty"`
	HealthFailures     int               `json:"health_failures,omitempty"`
	RestartOnUnhealthy bool              `json:"restart_on_unhealthy,omitempty"`
	Network            string            `json:"network,omitempty"`
	Identity           string            `json:"identity,omitempty"`
	Maintenance        bool              `json:"maintenance,omitempty"`
	DependsOn          []string          `json:"depends_on,omitempty"`
	JobConcurrency     int               `json:"job_concurrency,omitempty"`
	AllowCallers       []string          `json:"allow_callers,omitempty"`
	Egress             []egressRule      `json:"egress,omitempty"`
	Schedule           []scheduleConfig  `json:"schedule,omitempty"`
	Dev                *appDevConfig     `json:"dev,omitempty"`

	requestTimeout time.Duration
	startupTimeout time.Duration
	healthInterval time.Duration
}

// appDevConfig is the optional SvelteKit dev loop configuration carried in app.json.
// The supervisor does not use it, but keeps it so a config PATCH does not drop it.
type appDevConfig struct {
	Web     string   `json:"web,omitempty"`
	Command string   `json:"command,omitempty"`
	Build   string   `json:"build,omitempty"`
	Proxy   []string `json:"proxy,omitempty"`
	Watch   []string `json:"watch,omitempty"`
	Port    int      `json:"port,omitempty"`
}

// egressRule allows one IPv4 CIDR and TCP port for an untrusted app. The app's uid still reaches only these destinations plus storage and the internal fetch listener.
type egressRule struct {
	CIDR string `json:"cidr"`
	Port int    `json:"port"`
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
	name, dir, digest string
	port              int
	uid               int
	cfg               appConfig
	proxy             *httputil.ReverseProxy
	started           time.Time
	delay             time.Duration
	timer             *time.Timer
	logs              *appLog
	stats             appStats
	disk              diskUsageCache
	history           diskUsageCache
	requests          sync.WaitGroup // in-flight proxied requests, drained before a replaced version stops
	accessLines       atomic.Uint64

	mu               sync.Mutex
	scheduleCancel   context.CancelFunc
	cmd              *exec.Cmd
	done             chan struct{}
	exited, retired  bool
	starting         bool
	draining         bool
	unhealthyRestart bool

	slots          chan struct{}
	wasmEpoch      bool
	workPID        int
	healthOK       bool
	ready          bool
	healthFailures int
	nextHealth     time.Time
	lastHealthy    time.Time
	lastError      string
}

type supervisor struct {
	cfg            config
	storage        *storageServer
	backups        *backupManager
	fetch          *appFetchServer
	events         *hub
	deployments    *deploymentManager
	blobs          *blobStore
	identity       *identityStore
	bus            *eventBus
	secrets        *secretStore
	jobs           *jobManager
	metrics        *metrics
	auth           *authStore
	audit          *auditStore
	uids           *uidAllocator
	cgroups        *cgroupManager
	egress         *egressManager
	quota          *dataQuota
	runner         *runnerClient
	runnerLiveness *os.File
	runnerSnapshot []runnerAppInfo

	mu          sync.RWMutex
	apps, hosts map[string]*app

	deployLocks     appLocks
	deployRunsMu    sync.Mutex
	deployRuns      map[string]*deploymentRun
	artifactCacheMu sync.Mutex
	loginMu         sync.Mutex
	login           *loginLimiter
	identityLoginMu sync.Mutex
	identityLogin   *loginLimiter
	stopping        atomic.Bool
	ready           atomic.Bool

	versionOnce sync.Once
	version     string
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
	lastError := a.lastError
	info.CurrentDigest = a.digest
	info.Ready = a.ready
	info.Maintenance = a.cfg.Maintenance
	info.UID = a.uid
	if a.cmd != nil && a.cmd.Process != nil && !a.exited {
		info.PID = a.cmd.Process.Pid
		if a.workPID > 0 {
			info.PID = a.workPID
		}
	}
	a.mu.Unlock()
	info.LastError = s.redactAppText(a.name, lastError)
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
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	snapshot := s.snapshot()
	if !identity.isAdmin() {
		filtered := snapshot[:0]
		for _, info := range snapshot {
			if identity.canAccessApp(info.Name) {
				filtered = append(filtered, info)
			}
		}
		snapshot = filtered
	}
	writeJSON(w, http.StatusOK, snapshot)
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
	UID            int       `json:"uid,omitempty"`
	Status         string    `json:"status"`
	PID            int       `json:"pid,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	CPUPercent     float64   `json:"cpuPercent"`
	MemoryBytes    int64     `json:"memoryBytes"`
	DiskBytes      int64     `json:"diskBytes"`
	DiskLimitBytes int64     `json:"diskLimitBytes,omitempty"`
	HistoryBytes   int64     `json:"historyBytes"`
	OverQuota      bool      `json:"overQuota,omitempty"`
	Maintenance    bool      `json:"maintenance,omitempty"`
	SampledAt      time.Time `json:"sampledAt"`
	LastHealthy    time.Time `json:"lastHealthy"`
	LastError      string    `json:"lastError,omitempty"`
	CurrentDigest  string    `json:"currentDigest,omitempty"`
	Ready          bool      `json:"ready"`
}

func (s *supervisor) stopAll() {
	s.deployRunsMu.Lock()
	for _, run := range s.deployRuns {
		run.cancel()
	}
	s.deployRunsMu.Unlock()
	apps := s.allApps()

	var wg sync.WaitGroup
	for _, a := range apps {
		wg.Add(1)
		go func(a *app) {
			defer wg.Done()
			s.drainAndStop(a, "shutdown")
			if a.logs != nil {
				a.logs.close()
			}
		}(a)
	}
	wg.Wait()
}

// handleAppRoot serves the app resource itself: inspect, deploy, or delete.
func (s *supervisor) handleAppRoot(w http.ResponseWriter, r *http.Request, name string) {
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
}

// handleAppLogs serves either the log tail or the live log stream.
func (s *supervisor) handleAppLogs(w http.ResponseWriter, r *http.Request, name, action string) {
	a, ok := s.requireAppMethod(w, r, name, http.MethodGet)
	if !ok {
		return
	}
	if action == "logs/stream" {
		s.handleLogStream(w, r, a)
		return
	}
	s.handleLogs(w, r, a)
}

// handleAppDatabase dispatches the database endpoints for one app.
func (s *supervisor) handleAppDatabase(w http.ResponseWriter, r *http.Request, name, action string) {
	switch action {
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
	}
}

// requireAppMethod checks an app route's method before looking the app up.
func (s *supervisor) requireAppMethod(w http.ResponseWriter, r *http.Request, name, method string) (*app, bool) {
	if !requireMethod(w, r, method) {
		return nil, false
	}
	return s.requireApp(w, r, name)
}

// parseAppRoute splits /apps/<name>/<action> and rejects invalid names.
func parseAppRoute(path string) (name, action string, ok bool) {
	rest := strings.TrimPrefix(path, "/apps/")
	name, action, _ = strings.Cut(rest, "/")
	return name, action, name != "" && validName(name)
}

// handleAppPrefixRoute serves the routes whose action carries a subpath.
func (s *supervisor) handleAppPrefixRoute(w http.ResponseWriter, r *http.Request, name, action string) bool {
	if action == "jobs" || strings.HasPrefix(action, "jobs/") {
		s.handleAppJobs(w, r, name, action)
		return true
	}
	if action == "backups" || strings.HasPrefix(action, "backups/") {
		s.handleBackupRoute(w, r, name, strings.TrimPrefix(action, "backups"))
		return true
	}
	if id, ok := strings.CutPrefix(action, "deployments/"); ok {
		s.handleDeploymentRoute(w, r, name, id)
		return true
	}
	if action == "validate" {
		if requireMethod(w, r, http.MethodPost) {
			r.URL.RawQuery = "dry_run=1"
			s.deploy(w, r, name)
		}
		return true
	}
	if id, ok := strings.CutPrefix(action, "rollback/"); ok {
		if requireMethod(w, r, http.MethodPost) {
			s.handleRollback(w, r, name, id)
		}
		return true
	}
	if key, ok := strings.CutPrefix(action, "secrets/"); ok {
		s.handleAppSecrets(w, r, name, key)
		return true
	}
	return false
}

// handleAppVerb serves the routes whose action is a single word.
func (s *supervisor) handleAppVerb(w http.ResponseWriter, r *http.Request, name, action string) {
	switch action {
	case "":
		s.handleAppRoot(w, r, name)
	case "config":
		s.handleAppConfig(w, r, name)
	case "maintenance":
		s.handleAppMaintenance(w, r, name)
	case "secrets":
		s.handleAppSecrets(w, r, name, "")
	case "deployments":
		switch r.Method {
		case http.MethodGet:
			s.handleDeployments(w, r, name)
		case http.MethodPost:
			s.handleDeploymentCreate(w, r, name)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "stop":
		if requireMethod(w, r, http.MethodPost) {
			s.handleStop(w, r, name)
		}
	case "start":
		if requireMethod(w, r, http.MethodPost) {
			s.handleStart(w, r, name)
		}
	case "logs/stream", "logs":
		s.handleAppLogs(w, r, name, action)
	case "logs/export":
		if a, ok := s.requireAppMethod(w, r, name, http.MethodGet); ok {
			s.handleLogExport(w, r, a)
		}
	case "resources":
		if a, ok := s.requireAppMethod(w, r, name, http.MethodGet); ok {
			s.handleResources(w, r, a)
		}
	case "database", "database/rows", "database/changes", "database/export", "database/import", "database/maintenance":
		s.handleAppDatabase(w, r, name, action)
	case "restart":
		if a, ok := s.requireAppMethod(w, r, name, http.MethodPost); ok {
			s.handleRestart(w, r, a)
		}
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

// handleAppAction picks the subresource route before falling back to a single-word action.
func (s *supervisor) handleAppAction(w http.ResponseWriter, r *http.Request, name, action string) {
	if s.handleAppPrefixRoute(w, r, name, action) {
		return
	}
	s.handleAppVerb(w, r, name, action)
}

// handleAppAudited runs a mutating app request and appends its outcome to the audit log.
func (s *supervisor) handleAppAudited(w http.ResponseWriter, r *http.Request, identity authIdentity, name, action string) {
	recorder := &auditWriter{ResponseWriter: w}
	s.handleAppAction(recorder, r, name, action)
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	s.appendAudit(identity, r, auditAction(r.Method, action), name, status, auditError(recorder.body.Bytes()))
}

func (s *supervisor) handleApp(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	name, action, ok := parseAppRoute(r.URL.Path)
	if !ok {
		http.Error(w, "invalid app name", http.StatusBadRequest)
		return
	}
	if !identity.canAccessApp(name) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !identity.canPerformAppAction(r, action) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.audit != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.handleAppAudited(w, r, identity, name, action)
		return
	}
	s.handleAppAction(w, r, name, action)
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

// validateAppFetchListen keeps the internal app-to-app endpoint on loopback, where apps reach it through their egress allow rules.
func validateAppFetchListen(value string) error {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("app fetch listen %q: use a loopback address such as 127.0.0.1:0", value)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("app fetch listen %q must be loopback", value)
	}
	return nil
}

// parseConfig reads flags and environment, then prepares the runtime directories.
// startSupervisor builds the storage service, recovers interrupted rollbacks and loads every app.
func startSupervisor(cfg config) *supervisor {
	storageDir := filepath.Join(cfg.stateDir, ".storage")
	var storage *storageServer
	var err error
	if listener, ok, listenErr := inheritedListener(storageFDEnv); listenErr != nil {
		log.Fatalf("storage listener: %v", listenErr)
	} else if ok {
		storage, err = newStorageServerOn(storageDir, listener)
	} else {
		storage, err = newStorageServer(storageDir, cfg.storageListen)
	}
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	if err := storage.setTempDir(cfg.storageTmpDir, cfg.storageTempBudgetBytes); err != nil {
		log.Fatalf("storage temp: %v", err)
	}
	if cfg.storageMigrationTimeout > 0 {
		storage.migrationTimeout = cfg.storageMigrationTimeout
	}
	blobs, err := newBlobStore(cfg.stateDir)
	if err != nil {
		log.Fatalf("blobs: %v", err)
	}
	identity, err := newIdentityStore(cfg.stateDir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	secrets, err := newSecretStore(filepath.Join(cfg.stateDir, secretDirName))
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}
	auth, err := newAuthStore(tokenFilePath(cfg))
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	audit, err := newAuditStore(filepath.Join(cfg.stateDir, ".audit", "audit.log"))
	if err != nil {
		log.Fatalf("audit: %v", err)
	}
	s := &supervisor{
		cfg:         cfg,
		storage:     storage,
		backups:     newBackupManager(filepath.Join(cfg.stateDir, ".backups"), storage, cfg.stateDir),
		events:      newHub(),
		deployments: newDeploymentManager(filepath.Join(cfg.stateDir, ".deployments")),
		blobs:       blobs,
		identity:    identity,
		secrets:     secrets,
		auth:        auth,
		audit:       audit,
		apps:        map[string]*app{},
		hosts:       map[string]*app{},
	}
	s.bus = newEventBus()
	s.jobs = newJobManager(filepath.Join(cfg.stateDir, ".jobs"), s)
	s.metrics = newMetrics()
	storage.metrics = s.metrics
	storage.bus = s.bus
	var fetch *appFetchServer
	if listener, ok, listenErr := inheritedListener(fetchFDEnv); listenErr != nil {
		log.Fatalf("app fetch listener: %v", listenErr)
	} else if ok {
		fetch, err = newAppFetchServerOn(listener, s)
	} else {
		fetch, err = newAppFetchServer(cfg.appFetchListen, s)
	}
	if err != nil {
		log.Fatalf("app fetch: %v", err)
	}
	s.fetch = fetch
	if err := s.startRunner(); err != nil {
		log.Fatalf("runner: %v", err)
	}
	for _, info := range s.runnerSnapshot {
		s.storage.restoreToken(info.Name, info.StorageToken)
	}
	if cfg.untrusted() {
		uids, err := newUIDAllocator(filepath.Join(cfg.stateDir, ".uids.json"), cfg.appUIDBase)
		if err != nil {
			log.Fatalf("uid allocator: %v", err)
		}
		s.uids = uids
		storageIP, storagePort := storage.endpoint()
		s.cgroups = newCgroupManager(cfg.cgroupRoot)
		s.egress = newEgressManager(cfg.iptables, cfg.ip6tables, storageIP, storagePort, nil)
		s.egress.ipv6 = cfg.ipv6Egress
		fetchIP, fetchPort := fetch.endpoint()
		if ip := net.ParseIP(fetchIP); ip == nil || ip.To4() == nil {
			log.Fatalf("app fetch listener %s must use IPv4 in untrusted mode", fetchIP)
		}
		s.egress.setInternal(fetchIP, fetchPort)
		s.quota = newDataQuota(cfg.dataQuotaMode, cfg.dataQuotaFS, cfg.stateDir, cfg.setquota, cfg.findmnt, nil)
	}
	if err := recoverRollbacks(cfg.stateDir, s.backups); err != nil {
		log.Fatalf("rollback recovery: %v", err)
	}
	if cfg.appNamespaces {
		if err := resetNamespaceRoots(cfg.stateDir, cfg.untrusted()); err != nil {
			log.Fatalf("%v", err)
		}
	}
	if err := s.loadApps(); err != nil {
		log.Fatalf("load apps: %v", err)
	}
	s.ready.Store(true)
	return s
}

// shutdownServer stops accepting connections and waits for in-flight handlers, forcing the server closed at the deadline.
func shutdownServer(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
		return err
	}
	return nil
}

// serve runs the HTTP server and background monitors until a signal arrives.
func serve(s *supervisor) {
	cfg := s.cfg
	backupCtx, stopBackups := context.WithCancel(context.Background())
	defer stopBackups()
	go s.backups.loop(backupCtx)
	go s.storage.monitorTemp(backupCtx)
	go s.monitorResources(resourceSampleInterval)
	go s.monitorHealth()
	go s.monitorDiskBudget()
	if cfg.untrusted() {
		go s.monitorDiskQuota()
	}

	// When admin routes have their own listener, the public one serves app hosts and /healthz only.
	publicOnly := cfg.adminListen != ""
	handler := s.handler()
	if publicOnly {
		handler = s.publicHandler()
	}
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	var adminServer *http.Server
	var adminCleanup func()
	if publicOnly {
		network, address, err := parseAdminListen(cfg.adminListen)
		if err != nil {
			log.Fatalf("admin listen: %v", err)
		}
		listener, cleanup, err := listenAdmin(network, address)
		if err != nil {
			log.Fatalf("admin listen: %v", err)
		}
		adminCleanup = cleanup
		adminServer = &http.Server{
			Handler:           s.adminHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
		go func() {
			if err := adminServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("admin listen: %v", err)
			}
		}()
		log.Printf("admin listener on %s with %d app(s)", cfg.adminListen, len(s.apps))
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range stop {
			if sig == syscall.SIGHUP {
				if err := s.handover(); err != nil {
					s.stopping.Store(false)
					log.Printf("handover: %v", err)
				}
				continue
			}
			s.stopping.Store(true)
			s.ready.Store(false)
			log.Printf("shutting down")
			if adminServer != nil {
				if err := shutdownServer(adminServer, shutdownTimeout); err != nil {
					log.Printf("graceful admin shutdown: %v", err)
				}
			}
			if err := shutdownServer(server, shutdownTimeout); err != nil {
				log.Printf("graceful shutdown: %v", err)
			}
			return
		}
	}()

	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatalf("--tls-cert and --tls-key must be set together")
	}
	log.Printf("supervisor listening on %s with %d app(s)", cfg.listen, len(s.apps))
	var err error
	if cfg.tlsCert != "" {
		err = server.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	if adminServer != nil {
		_ = shutdownServer(adminServer, shutdownTimeout)
	}
	s.stopAll()
	if s.runnerLiveness != nil {
		_ = s.runnerLiveness.Close()
		s.runnerLiveness = nil
	}
	if s.runner != nil {
		s.runner.Close()
	}
	if s.egress != nil {
		if err := s.egress.shutdown(); err != nil {
			log.Printf("egress shutdown: %v", err)
		}
	}
	if s.cgroups != nil {
		s.cgroups.close()
	}
	s.persistResources()
	s.jobs.close()
	s.fetch.close()
	s.storage.close()
	if adminCleanup != nil {
		adminCleanup()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == runnerArg {
		runnerMain()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == nsInitArg {
		nsInitMain(os.Args[2:])
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	serve(startSupervisor(parseConfig()))
}
