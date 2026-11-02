package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxStorageValue      = 1 << 20
	maxStorageBody       = 8 << 20
	maxStorageSQL        = 64 << 10
	maxStorageParams     = 128
	maxStorageRows       = 10_000
	maxStorageStatements = 64
	maxStorageColumns    = 256
	maxStorageResult     = 8 << 20
	maxStorageSessions   = 8
	maxStorageReadConns  = 4
	storageTimeout       = 5 * time.Second
	storageSessionLease  = 30 * time.Second
	// Migrations may build indexes or backfill rows, so they get their own longer and configurable deadline.
	defaultStorageMigrationTimeout = 5 * time.Minute
	minStorageMigrationTimeout     = time.Second
	maxStorageMigrationTimeout     = time.Hour
	// maxStorageConcurrentQueries bounds all apps' storage work together, so many tenants cannot exhaust the supervisor.
	maxStorageConcurrentQueries = 16
	storageQueueTimeout         = 5 * time.Second
	// storagePageSize is SQLite's default page size for a new database. Per-app max_page_count is derived from it.
	storagePageSize = 4096
	// storageWALAutoCheckpoint checkpoints the WAL after this many pages, keeping the WAL near 4 MiB between checkpoints.
	storageWALAutoCheckpoint = 1000
	// journalSizeLimit bounds the WAL after a checkpoint. It is capped at 64 MiB and scaled down for small quotas.
	defaultJournalSizeLimit = 64 << 20
	minJournalSizeLimit     = 256 << 10
	// storageTempMonitorInterval bounds how often the global SQLite temp directory is measured.
	storageTempMonitorInterval = 10 * time.Second
	// defaultStorageTempBudgetMB is the default global temp directory budget.
	defaultStorageTempBudgetMB = 512
)

var (
	errStorageUnavailable = errors.New("storage unavailable")
	errStorageInvalid     = errors.New("invalid storage request")
	errStorageTempBudget  = errors.New("storage temp budget exceeded")
)

// storageServer is a tiny per-app SQLite API. It listens on loopback and authenticates apps with a token, so app processes never touch the database files directly.
type storageServer struct {
	dir, baseURL string
	listener     net.Listener
	server       *http.Server

	mu, changeMu sync.Mutex
	// opMu blocks new queries while a restore or quota change replaces a database pool; normal queries share it as readers.
	opMu sync.RWMutex
	// quotas is the per-app database byte quota; zero means unlimited. Guarded by mu.
	quotas map[string]int64
	tokens map[string]string // token -> app name
	dbs    map[string]*sql.DB
	// readDBs are query-only pools. They let /v1/query run without any write capability, even a mistaken one.
	readDBs map[string]*sql.DB

	// tempDir is the process-wide SQLite temp directory; tempBudget is its byte budget, zero for unlimited.
	// tempOver is set by the monitor and rejects new storage requests until usage falls back under the budget.
	tempDir    string
	tempBudget *byteBudget
	tempOver   atomic.Bool

	sessionMu      sync.Mutex
	sessionCond    *sync.Cond
	sessions       map[string]*storageSession
	sessionsPaused int
	sessionClosed  bool
	sessionStop    chan struct{}
	sessionDone    chan struct{}

	migrationTimeout time.Duration

	querySem chan struct{}

	metrics *metrics
	bus     *eventBus

	routesCache routeCache
}

type storageRequest struct {
	SQL     string           `json:"sql"`
	Params  []jsontext.Value `json:"params,omitempty"`
	MaxRows int              `json:"max_rows,omitempty"`
}

type storageResult struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Truncated    bool     `json:"truncated"`
}

// storageStatement is one SQL statement in a batch, transaction or migration.
type storageStatement struct {
	SQL    string           `json:"sql"`
	Params []jsontext.Value `json:"params,omitempty"`
	// Query marks statements whose rows should be returned; otherwise the result carries rows_affected and last_insert_id.
	Query bool `json:"query,omitempty"`
}

type storageStatementsRequest struct {
	Statements []storageStatement `json:"statements"`
}

type storageStatementsResponse struct {
	Results []storageResult `json:"results"`
}

func newStorageServer(dir, listen string) (*storageServer, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("storage listen: %w", err)
	}
	return newStorageServerOn(dir, listener)
}

func newStorageServerOn(dir string, listener net.Listener) (*storageServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage dir: %w", err)
	}

	s := &storageServer{
		dir:         dir,
		baseURL:     "http://" + listener.Addr().String(),
		quotas:      map[string]int64{},
		tokens:      map[string]string{},
		dbs:         map[string]*sql.DB{},
		readDBs:     map[string]*sql.DB{},
		sessions:    map[string]*storageSession{},
		sessionStop: make(chan struct{}),
		sessionDone: make(chan struct{}),
		querySem:    make(chan struct{}, maxStorageConcurrentQueries),

		migrationTimeout: defaultStorageMigrationTimeout,
	}
	s.sessionCond = sync.NewCond(&s.sessionMu)
	s.listener = listener
	s.server = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("storage: %v", err)
		}
	}()
	go s.sessionLoop()
	return s, nil
}

// cleanStorageTemp removes stale SQLite temp files from a previous crash, leaving unrelated files alone.
func cleanStorageTemp(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "etilqs_") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// setTempDir points SQLite at a dedicated temp directory and records its global budget.
// SQLite's temp directory is process-wide, so every app shares it; the per-app quota cannot attribute these files.
func (s *storageServer) setTempDir(dir string, budget int64) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("storage temp dir: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("storage temp dir permissions: %w", err)
	}
	if err := cleanStorageTemp(dir); err != nil {
		return fmt.Errorf("clean storage temp dir: %w", err)
	}
	s.mu.Lock()
	s.tempDir = dir
	s.tempBudget = &byteBudget{name: "storage temp", limit: max(budget, 0)}
	s.mu.Unlock()
	s.tempOver.Store(false)
	return nil
}

// dirSize sums the regular files in a directory without following links.
func dirSize(dir string) (int64, error) {
	var total, entries int64
	if err := walkUsage(dir, &total, &entries); err != nil {
		return 0, err
	}
	return total, nil
}

// checkTemp updates the over-budget flag used to reject new storage requests. Existing queries finish under their own five-second deadline.
func (s *storageServer) checkTemp() {
	s.mu.Lock()
	dir := s.tempDir
	budget := s.tempBudget
	s.mu.Unlock()
	if dir == "" || !budget.enabled() {
		s.tempOver.Store(false)
		if budget != nil {
			budget.observe(0)
		}
		return
	}
	size, err := dirSize(dir)
	if err != nil {
		log.Printf("storage temp: measure %s: %v", dir, err)
		return
	}
	s.tempOver.Store(budget.observe(size))
}

// monitorTemp refreshes the temp directory budget until ctx is cancelled.
func (s *storageServer) monitorTemp(ctx context.Context) {
	s.checkTemp()
	ticker := time.NewTicker(storageTempMonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkTemp()
		}
	}
}

func (s *storageServer) url() string {
	return s.baseURL
}

// endpoint returns the address apps and egress rules use to reach the storage service.
func (s *storageServer) endpoint() (string, int) {
	address, ok := s.listener.Addr().(*net.TCPAddr)
	if !ok || address == nil {
		return "", 0
	}
	ip := address.IP
	if ip == nil || ip.IsUnspecified() {
		return "127.0.0.1", address.Port
	}
	return ip.String(), address.Port
}

func (s *storageServer) close() {
	_ = s.server.Close()
	close(s.sessionStop)
	<-s.sessionDone
	s.sessionMu.Lock()
	s.sessionClosed = true
	s.sessionCond.Broadcast()
	s.sessionMu.Unlock()
	s.closeSessionsFor("")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, db := range s.dbs {
		_ = db.Close()
	}
	for _, db := range s.readDBs {
		_ = db.Close()
	}
}

// drop closes an app's database and forgets its token, so its files can be removed.
func (s *storageServer) drop(app string) {
	s.mu.Lock()
	db := s.dbs[app]
	readDB := s.readDBs[app]
	delete(s.dbs, app)
	delete(s.readDBs, app)
	delete(s.quotas, app)
	for token, name := range s.tokens {
		if name == app {
			delete(s.tokens, token)
		}
	}
	s.mu.Unlock()
	if readDB != nil {
		_ = readDB.Close()
	}
	if db != nil {
		_ = db.Close()
	}
}

// tokenFor returns the app's stable token for this supervisor run.
func (s *storageServer) tokenFor(app string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, name := range s.tokens {
		if name == app {
			return token
		}
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	s.tokens[token] = app
	return token
}

// restoreToken carries an app's storage token across a supervisor handover.
// The running app's env already contains it, so the new storage service must accept the same value.
func (s *storageServer) restoreToken(app, token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	s.tokens[token] = app
	s.mu.Unlock()
}

// appForToken resolves an app from a bearer token.
func (s *storageServer) appForToken(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.tokens[token]
	return app, ok
}

// appFor resolves the app from the request's bearer token.
func (s *storageServer) appFor(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), prefix)
	if !ok {
		return "", false
	}
	return s.appForToken(token)
}
