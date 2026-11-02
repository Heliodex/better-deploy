package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// newStorageForConfig opens the storage server on an inherited or configured listener.
func newStorageForConfig(cfg config) *storageServer {
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
	return storage
}

// newFetchServer opens the internal app-to-app listener on an inherited or configured address.
func newFetchServer(cfg config, s *supervisor) *appFetchServer {
	if listener, ok, listenErr := inheritedListener(fetchFDEnv); listenErr != nil {
		log.Fatalf("app fetch listener: %v", listenErr)
	} else if ok {
		fetch, err := newAppFetchServerOn(listener, s)
		if err != nil {
			log.Fatalf("app fetch: %v", err)
		}
		return fetch
	}
	fetch, err := newAppFetchServer(cfg.appFetchListen, s)
	if err != nil {
		log.Fatalf("app fetch: %v", err)
	}
	return fetch
}

// newSupervisorServices opens the persistent stores and internal listeners the supervisor owns.
func newSupervisorServices(cfg config) *supervisor {
	storage := newStorageForConfig(cfg)
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
	storage.jobs = s.jobs
	s.fetch = newFetchServer(cfg, s)
	return s
}

// configureUntrusted installs the per-app uid, cgroup, egress and quota boundaries.
func configureUntrusted(s *supervisor, cfg config) {
	uids, err := newUIDAllocator(filepath.Join(cfg.stateDir, ".uids.json"), cfg.appUIDBase)
	if err != nil {
		log.Fatalf("uid allocator: %v", err)
	}
	s.uids = uids
	storageIP, storagePort := s.storage.endpoint()
	s.cgroups = newCgroupManager(cfg.cgroupRoot)
	s.egress = newEgressManager(cfg.iptables, cfg.ip6tables, storageIP, storagePort, nil)
	s.egress.ipv6 = cfg.ipv6Egress
	fetchIP, fetchPort := s.fetch.endpoint()
	if ip := net.ParseIP(fetchIP); ip == nil || ip.To4() == nil {
		log.Fatalf("app fetch listener %s must use IPv4 in untrusted mode", fetchIP)
	}
	s.egress.setInternal(fetchIP, fetchPort)
	s.quota = newDataQuota(cfg.dataQuotaMode, cfg.dataQuotaFS, cfg.stateDir, cfg.setquota, cfg.findmnt, nil)
}

// startSupervisor builds the services, recovers interrupted work and loads every app.
func startSupervisor(cfg config) *supervisor {
	s := newSupervisorServices(cfg)
	if err := s.startRunner(); err != nil {
		log.Fatalf("runner: %v", err)
	}
	for _, info := range s.runnerSnapshot {
		s.storage.restoreToken(info.Name, info.StorageToken)
	}
	if cfg.untrusted() {
		configureUntrusted(s, cfg)
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

// startMonitors launches the backup, storage and resource monitors.
func (s *supervisor) startMonitors(ctx context.Context) {
	go s.backups.loop(ctx)
	go s.storage.monitorTemp(ctx)
	go s.monitorResources(resourceSampleInterval)
	go s.monitorHealth()
	go s.monitorDiskBudget()
	if s.cfg.untrusted() {
		go s.monitorDiskQuota()
	}
}

// newHTTPServer builds the public or admin listener with the shared timeout policy.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// startAdminServer serves the admin API on its own listener and returns its cleanup.
func startAdminServer(s *supervisor, cfg config) (*http.Server, func()) {
	if cfg.adminListen == "" {
		return nil, nil
	}
	network, address, err := parseAdminListen(cfg.adminListen)
	if err != nil {
		log.Fatalf("admin listen: %v", err)
	}
	listener, cleanup, err := listenAdmin(network, address)
	if err != nil {
		log.Fatalf("admin listen: %v", err)
	}
	adminServer := newHTTPServer("", s.adminHandler())
	go func() {
		if err := adminServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("admin listen: %v", err)
		}
	}()
	log.Printf("admin listener on %s with %d app(s)", cfg.adminListen, len(s.apps))
	return adminServer, cleanup
}

// listenForSignals handles SIGHUP handover and graceful shutdown on SIGINT or SIGTERM.
func (s *supervisor) listenForSignals(server, adminServer *http.Server) {
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
}

// listenAndServe starts the public listener with TLS when configured.
func listenAndServe(server *http.Server, s *supervisor) error {
	cfg := s.cfg
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatalf("--tls-cert and --tls-key must be set together")
	}
	log.Printf("supervisor listening on %s with %d app(s)", cfg.listen, len(s.apps))
	if cfg.tlsCert != "" {
		return server.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	}
	return server.ListenAndServe()
}

// stopServices drains the listeners and releases every process, rule and store.
func (s *supervisor) stopServices(adminServer *http.Server, adminCleanup func(), err error) {
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

// serve runs the HTTP server and background monitors until a signal arrives.
func serve(s *supervisor) {
	cfg := s.cfg
	monitorCtx, stopMonitors := context.WithCancel(context.Background())
	defer stopMonitors()
	s.startMonitors(monitorCtx)

	// When admin routes have their own listener, the public one serves app hosts and /healthz only.
	handler := s.handler()
	if cfg.adminListen != "" {
		handler = s.publicHandler()
	}
	server := newHTTPServer(cfg.listen, handler)
	adminServer, adminCleanup := startAdminServer(s, cfg)
	s.listenForSignals(server, adminServer)
	err := listenAndServe(server, s)
	s.stopServices(adminServer, adminCleanup, err)
}
