package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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
	tempBudget int64
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

type storageStatement struct {
	SQL    string           `json:"sql"`
	Params []jsontext.Value `json:"params,omitempty"`
}

type storageBatchRequest struct {
	Statements []storageStatement `json:"statements"`
}

type storageBatchResponse struct {
	Results []storageResult `json:"results"`
}

func newStorageServer(dir, listen string) (*storageServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage dir: %w", err)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("storage listen: %w", err)
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
	s.tempBudget = max(budget, 0)
	s.mu.Unlock()
	s.tempOver.Store(false)
	return nil
}

// dirSize sums the regular files in a directory without following links.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// checkTemp updates the over-budget flag used to reject new storage requests. Existing queries finish under their own five-second deadline.
func (s *storageServer) checkTemp() {
	s.mu.Lock()
	dir := s.tempDir
	budget := s.tempBudget
	s.mu.Unlock()
	if dir == "" || budget <= 0 {
		s.tempOver.Store(false)
		return
	}
	size, err := dirSize(dir)
	if err != nil {
		log.Printf("storage temp: measure %s: %v", dir, err)
		return
	}
	over := size > budget
	if over != s.tempOver.Swap(over) {
		if over {
			log.Printf("storage temp: %s is over its %d-byte budget", dir, budget)
		} else {
			log.Printf("storage temp: %s is under its budget again", dir)
		}
	}
}

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

func (s *storageServer) appFor(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), prefix)
	if !ok {
		return "", false
	}
	return s.appForToken(token)
}

// storageDSN applies the hardened pragmas to every physical connection through the driver DSN.
// The write pool owns WAL and starts every transaction with BEGIN IMMEDIATE, while both pools get busy_timeout, foreign keys and synchronous=NORMAL.
// A positive quota becomes SQLite's per-database max_page_count, assuming the default 4096-byte page size.
// tempDir sets SQLite's process-wide temp directory; all apps share it.
func storageDSN(path string, readOnly bool, quotaBytes int64, tempDir string) string {
	dsn := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_synchronous", "NORMAL")
	if !readOnly {
		query.Set("_journal_mode", "WAL")
		query.Set("_txlock", "immediate")
	}
	query.Add("_pragma", "trusted_schema(OFF)")
	query.Add("_pragma", "temp_store(FILE)")
	query.Add("_pragma", "cache_size(-2048)")
	query.Add("_pragma", "mmap_size(0)")
	if tempDir != "" {
		// temp_store_directory is deprecated but remains the only in-process way to choose SQLite's temp directory.
		query.Add("_pragma", "temp_store_directory("+sqliteString(tempDir)+")")
	}
	if !readOnly {
		query.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", journalSizeLimit(quotaBytes)))
		query.Add("_pragma", fmt.Sprintf("wal_autocheckpoint(%d)", walAutoCheckpoint(quotaBytes)))
	}
	if quotaBytes > 0 {
		pages := quotaBytes / storagePageSize
		if pages < 1 {
			pages = 1
		}
		query.Add("_pragma", fmt.Sprintf("max_page_count(%d)", pages))
	}
	if readOnly {
		query.Set("_query_only", "1")
	}
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

// journalSizeLimit scales the WAL truncation limit with the app's database quota, capped at 64 MiB.
func journalSizeLimit(quotaBytes int64) int64 {
	if quotaBytes <= 0 {
		return defaultJournalSizeLimit
	}
	limit := quotaBytes / 8
	if limit < minJournalSizeLimit {
		return minJournalSizeLimit
	}
	if limit > defaultJournalSizeLimit {
		return defaultJournalSizeLimit
	}
	return limit
}

// walAutoCheckpoint keeps the WAL to about a quarter of a small quota, with a 16-page floor and the default 1000-page ceiling.
func walAutoCheckpoint(quotaBytes int64) int {
	if quotaBytes <= 0 {
		return storageWALAutoCheckpoint
	}
	pages := quotaBytes / storagePageSize / 4
	if pages < 16 {
		return 16
	}
	if pages > storageWALAutoCheckpoint {
		return storageWALAutoCheckpoint
	}
	return int(pages)
}

// sqliteString quotes a value for a pragma call in the DSN, doubling embedded single quotes.
func sqliteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// parseStorageMigrationTimeout validates the host-wide deadline for a migration. It defaults to five minutes and stays within one second and one hour.
func parseStorageMigrationTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultStorageMigrationTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minStorageMigrationTimeout || timeout > maxStorageMigrationTimeout {
		return 0, fmt.Errorf("storage migration timeout %q must be a duration between %s and %s", value, minStorageMigrationTimeout, maxStorageMigrationTimeout)
	}
	return timeout, nil
}

// setQuota records an app's database byte quota and drops its pools when the value changes, so the next request opens them with the new max_page_count.
func (s *storageServer) setQuota(app string, bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	s.opMu.Lock()
	s.mu.Lock()
	if s.quotas == nil {
		s.quotas = map[string]int64{}
	}
	old, ok := s.quotas[app]
	if ok && old == bytes {
		s.mu.Unlock()
		s.opMu.Unlock()
		return
	}
	s.quotas[app] = bytes
	db := s.dbs[app]
	readDB := s.readDBs[app]
	delete(s.dbs, app)
	delete(s.readDBs, app)
	s.mu.Unlock()
	s.opMu.Unlock()

	if readDB != nil {
		_ = readDB.Close()
	}
	if db != nil {
		_ = db.Close()
	}
}

func (s *storageServer) dbFor(app string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.dbs[app]; db != nil {
		return db, nil
	}

	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("app storage dir: %w", err)
	}
	db, err := sql.Open("sqlite", storageDSN(filepath.Join(dir, "storage.db"), false, s.quotas[app], s.tempDir))
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	// WAL allows concurrent readers; SQLite still serialises writers. Sessions pin connections, so leave half the pool for ordinary queries.
	db.SetMaxOpenConns(2 * maxStorageSessions)
	db.SetMaxIdleConns(2 * maxStorageSessions)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s.dbs[app] = db
	return db, nil
}

// readDBFor opens a second pool in SQLite's query_only mode. /v1/query uses it so a bug in the authorizer still cannot write through the read path.
func (s *storageServer) readDBFor(app string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.readDBs[app]; db != nil {
		return db, nil
	}

	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("app storage dir: %w", err)
	}
	db, err := sql.Open("sqlite", storageDSN(filepath.Join(dir, "storage.db"), true, s.quotas[app], s.tempDir))
	if err != nil {
		return nil, fmt.Errorf("open read storage: %w", err)
	}
	db.SetMaxOpenConns(maxStorageReadConns)
	db.SetMaxIdleConns(maxStorageReadConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read storage: %w", err)
	}
	s.readDBs[app] = db
	return db, nil
}

// databasePath is the app's live SQLite file, outside the app's /data preopen.
func (s *storageServer) databasePath(app string) string {
	return filepath.Join(s.dir, app, "storage.db")
}

// hasDatabase reports whether the app has used storage yet. Checking the file avoids creating an empty database just to inspect it.
func (s *storageServer) hasDatabase(app string) bool {
	info, err := os.Stat(s.databasePath(app))
	return err == nil && info.Mode().IsRegular()
}

// sqliteBackup is the slice of modernc.org/sqlite's connection API used by the online backup calls.
type sqliteBackup interface {
	NewBackup(dst string) (*sqlite.Backup, error)
	NewRestore(src string) (*sqlite.Backup, error)
}

// runSQLiteBackup drives SQLite's online backup API to completion on one connection.
func runSQLiteBackup(conn *sql.Conn, start func(sqliteBackup) (*sqlite.Backup, error)) error {
	return conn.Raw(func(driverConn any) error {
		driver, ok := driverConn.(sqliteBackup)
		if !ok {
			return fmt.Errorf("sqlite driver does not support online backups")
		}
		backup, err := start(driver)
		if err != nil {
			return err
		}
		for {
			more, err := backup.Step(-1)
			if err != nil {
				_ = backup.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return backup.Finish()
	})
}

// checkBackupDatabase rejects a truncated or corrupt snapshot before it is restored over live data.
func checkBackupDatabase(path string) error {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), backupCheckTimeout)
	defer cancel()
	var result string
	if err := db.QueryRowContext(ctx, "pragma quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check: %s", result)
	}
	return nil
}

// configureStorageConn applies the per-connection safety limits that the DSN cannot express.
// ATTACH is disabled at the SQLite level, which also blocks VACUUM INTO and therefore arbitrary file writes from SQL.
// The pragmas themselves, including temp_store, cache_size, mmap_size and synchronous, are set in storageDSN for every physical connection.
func configureStorageConn(ctx context.Context, conn *sql.Conn) error {
	for _, limit := range []struct {
		id, value int
	}{
		{sqlite3.SQLITE_LIMIT_ATTACHED, 0},
		{sqlite3.SQLITE_LIMIT_SQL_LENGTH, maxStorageSQL},
		{sqlite3.SQLITE_LIMIT_LENGTH, maxStorageValue},
		{sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, maxStorageParams},
		{sqlite3.SQLITE_LIMIT_COLUMN, maxStorageColumns},
		{sqlite3.SQLITE_LIMIT_EXPR_DEPTH, 100},
		{sqlite3.SQLITE_LIMIT_COMPOUND_SELECT, 20},
	} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *storageServer) withConn(ctx context.Context, app string, fn func(*sql.Conn) error) error {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.withConnUnlocked(ctx, app, fn)
}

func (s *storageServer) withConnUnlocked(ctx context.Context, app string, fn func(*sql.Conn) error) (err error) {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.observeStorage(app, "write", err, time.Since(start))
		}
	}()
	db, err := s.dbFor(app)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	defer conn.Close()
	if err := configureStorageConn(ctx, conn); err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	return fn(conn)
}

// withReadConn runs a read on the query-only pool. It takes the same restore lock as the write pool, so replacing a database file waits for in-flight reads.
func (s *storageServer) withReadConn(ctx context.Context, app string, fn func(*sql.Conn) error) (err error) {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.observeStorage(app, "read", err, time.Since(start))
		}
	}()
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	db, err := s.readDBFor(app)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	defer conn.Close()
	if err := configureStorageConn(ctx, conn); err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	return fn(conn)
}

// backupTo writes a consistent snapshot to dst while the app keeps reading and writing.
func (s *storageServer) backupTo(ctx context.Context, app, dst string) error {
	return s.withConn(ctx, app, func(conn *sql.Conn) error {
		return runSQLiteBackup(conn, func(driver sqliteBackup) (*sqlite.Backup, error) {
			return driver.NewBackup(dst)
		})
	})
}

// restoreFrom copies a snapshot over the live database. It holds opMu exclusively, so no query is in flight while the copy runs.
func (s *storageServer) restoreFrom(ctx context.Context, app, src string) error {
	s.pauseSessions()
	defer s.resumeSessions()
	s.closeSessionsForPaused(app)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.withConnUnlocked(ctx, app, func(conn *sql.Conn) error {
		return runSQLiteBackup(conn, func(driver sqliteBackup) (*sqlite.Backup, error) {
			return driver.NewRestore(src)
		})
	})
}

func (s *storageServer) handleSQL(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fn(w, r)
}

func readStorageJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxStorageBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return false
		}
		writeStorageError(w, fmt.Errorf("%w: read body: %v", errStorageInvalid, err))
		return false
	}
	if len(data) == 0 {
		writeStorageError(w, fmt.Errorf("%w: empty body", errStorageInvalid))
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		writeStorageError(w, fmt.Errorf("%w: invalid JSON: %v", errStorageInvalid, err))
		return false
	}
	return true
}

func (s *storageServer) exec(w http.ResponseWriter, r *http.Request, app string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeStorageError(w, err)
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeStorageError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	var result storageResult
	var collector *rowChangeCollector
	err = s.withWriteConn(ctx, app, func(conn *sql.Conn, changes *rowChangeCollector) error {
		collector = changes
		unregister, err := requireStorageAuthorizer(conn, false)
		if err != nil {
			return err
		}
		defer func() { _ = unregister() }()
		res, err := conn.ExecContext(ctx, req.SQL, params...)
		if err != nil {
			return err
		}
		result.RowsAffected, _ = res.RowsAffected()
		result.LastInsertID, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var rows []rowChange
	truncated := false
	if collector != nil {
		rows = collector.rows
		truncated = collector.omitted > 0
	}
	s.logChanges(app, []storageChange{{
		Time:          time.Now().UTC(),
		RequestID:     r.Header.Get(requestIDHeader),
		SQL:           req.SQL,
		Params:        req.Params,
		RowsAffected:  result.RowsAffected,
		Rows:          rows,
		RowsTruncated: truncated,
	}})
	writeJSON(w, http.StatusOK, result)
}

func (s *storageServer) batch(w http.ResponseWriter, r *http.Request, app string) {
	var req storageBatchRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if len(req.Statements) == 0 {
		writeStorageError(w, fmt.Errorf("%w: at least one statement is required", errStorageInvalid))
		return
	}
	if len(req.Statements) > maxStorageStatements {
		writeStorageError(w, fmt.Errorf("%w: at most %d statements are allowed", errStorageInvalid, maxStorageStatements))
		return
	}
	params := make([][]any, len(req.Statements))
	for i, statement := range req.Statements {
		if err := validateStorageSQL(statement.SQL); err != nil {
			writeStorageError(w, fmt.Errorf("statement %d: %w", i+1, err))
			return
		}
		decoded, err := decodeStorageParams(statement.Params)
		if err != nil {
			writeStorageError(w, fmt.Errorf("statement %d: %w", i+1, err))
			return
		}
		params[i] = decoded
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	results := make([]storageResult, 0, len(req.Statements))
	starts := make([]int, len(req.Statements))
	ends := make([]int, len(req.Statements))
	var collector *rowChangeCollector
	err := s.withWriteConn(ctx, app, func(conn *sql.Conn, rows *rowChangeCollector) error {
		collector = rows
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		unregister, err := requireStorageAuthorizer(conn, false)
		if err != nil {
			return err
		}
		defer func() { _ = unregister() }()

		for i, statement := range req.Statements {
			starts[i] = len(rows.rows)
			res, err := tx.ExecContext(ctx, statement.SQL, params[i]...)
			if err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
			ends[i] = len(rows.rows)
			affected, _ := res.RowsAffected()
			lastID, _ := res.LastInsertId()
			results = append(results, storageResult{RowsAffected: affected, LastInsertID: lastID})
		}
		if err := unregister(); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	changes := make([]storageChange, len(req.Statements))
	for i, statement := range req.Statements {
		var rows []rowChange
		truncated := false
		if collector != nil {
			rows = collector.rows[starts[i]:ends[i]]
			truncated = collector.omitted > 0
		}
		changes[i] = storageChange{
			Time:          time.Now().UTC(),
			RequestID:     r.Header.Get(requestIDHeader),
			SQL:           statement.SQL,
			Params:        statement.Params,
			RowsAffected:  results[i].RowsAffected,
			Rows:          rows,
			RowsTruncated: truncated,
		}
	}
	s.logChanges(app, changes)
	writeJSON(w, http.StatusOK, storageBatchResponse{Results: results})
}

// readStorageRows scans and encodes a query result, stopping at maxRows or the result size limit.
func readStorageRows(rows *sql.Rows, maxRows int) (columns []string, encoded [][]any, truncated bool, err error) {
	columns, err = rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	if len(columns) > maxStorageColumns {
		return nil, nil, false, fmt.Errorf("query returns too many columns")
	}

	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	total := 0
	for rows.Next() {
		if len(encoded) >= maxRows {
			truncated = true
			break
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, false, err
		}
		size := 0
		for _, value := range values {
			size += storageValueSize(value)
		}
		if total+size > maxStorageResult {
			truncated = true
			break
		}
		total += size
		row := make([]any, len(values))
		for i, value := range values {
			row[i] = encodeStorageValue(value)
		}
		encoded = append(encoded, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return columns, encoded, truncated, nil
}

func (s *storageServer) query(w http.ResponseWriter, r *http.Request, app string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeStorageError(w, err)
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	maxRows := req.MaxRows
	if maxRows <= 0 || maxRows > maxStorageRows {
		maxRows = maxStorageRows
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	result := storageResult{Columns: []string{}, Rows: [][]any{}}
	err = s.withReadConn(ctx, app, func(conn *sql.Conn) error {
		unregister, err := requireStorageAuthorizer(conn, true)
		if err != nil {
			return err
		}
		defer func() { _ = unregister() }()

		rows, err := conn.QueryContext(ctx, req.SQL, params...)
		if err != nil {
			return err
		}
		defer rows.Close()

		result.Columns, result.Rows, result.Truncated, err = readStorageRows(rows, maxRows)
		return err
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeReturning runs a statement that both changes rows and returns them, such as INSERT ... RETURNING, and records the rows in the change log.
func (s *storageServer) writeReturning(w http.ResponseWriter, r *http.Request, app string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeStorageError(w, err)
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	maxRows := req.MaxRows
	if maxRows <= 0 || maxRows > maxStorageRows {
		maxRows = maxStorageRows
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	var collector *rowChangeCollector
	result := storageResult{Columns: []string{}, Rows: [][]any{}}
	err = s.withWriteConn(ctx, app, func(conn *sql.Conn, changes *rowChangeCollector) error {
		collector = changes
		unregister, err := requireStorageAuthorizer(conn, false)
		if err != nil {
			return err
		}
		defer func() { _ = unregister() }()

		rows, err := conn.QueryContext(ctx, req.SQL, params...)
		if err != nil {
			return err
		}
		result.Columns, result.Rows, result.Truncated, err = readStorageRows(rows, maxRows)
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return conn.QueryRowContext(ctx, "select changes()").Scan(&result.RowsAffected)
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}

	var rows []rowChange
	truncated := false
	if collector != nil {
		rows = collector.rows
		truncated = collector.omitted > 0
	}
	s.logChanges(app, []storageChange{{
		Time:          time.Now().UTC(),
		RequestID:     r.Header.Get(requestIDHeader),
		SQL:           req.SQL,
		Params:        req.Params,
		RowsAffected:  result.RowsAffected,
		Rows:          rows,
		RowsTruncated: truncated,
	}})
	writeJSON(w, http.StatusOK, result)
}

// acquireQuery takes one of the global storage slots, returning false when the service is saturated.
func (s *storageServer) acquireQuery(w http.ResponseWriter, r *http.Request) bool {
	if s.querySem == nil {
		return true
	}
	timer := time.NewTimer(storageQueueTimeout)
	defer timer.Stop()
	select {
	case s.querySem <- struct{}{}:
		return true
	case <-r.Context().Done():
		writeError(w, http.StatusServiceUnavailable, "storage busy")
		return false
	case <-timer.C:
		writeError(w, http.StatusServiceUnavailable, "storage busy")
		return false
	}
}

func (s *storageServer) handle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFor(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wt-storage"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.tempOver.Load() {
		writeStorageError(w, errStorageTempBudget)
		return
	}
	if !s.acquireQuery(w, r) {
		return
	}
	if s.querySem != nil {
		defer func() { <-s.querySem }()
	}

	if r.URL.Path == "/v1/sessions" {
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.createSession(w, r, app)
		})
		return
	}
	if id, action, ok := sessionAction(r.URL.Path); ok {
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			switch action {
			case "query":
				s.sessionQuery(w, r, app, id)
			case "exec":
				s.sessionExec(w, r, app, id)
			case "commit":
				s.closeSessionRequest(w, r, app, id, true)
			case "rollback":
				s.closeSessionRequest(w, r, app, id, false)
			}
		})
		return
	}

	switch r.URL.Path {
	case "/v1/query":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.query(w, r, app)
		})
	case "/v1/exec":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.exec(w, r, app)
		})
	case "/v1/write":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.writeReturning(w, r, app)
		})
	case "/v1/batch":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.batch(w, r, app)
		})
	case "/v1/transaction":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.transaction(w, r, app)
		})
	case "/v1/schema":
		s.readSchema(w, r, app)
	case "/v1/migrate":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.runMigration(w, r, app)
		})
	default:
		http.NotFound(w, r)
	}
}
