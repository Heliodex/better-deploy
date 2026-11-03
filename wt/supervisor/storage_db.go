package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

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

// readDBFor opens a second pool in SQLite's query_only mode. /v1/query uses it so a bug in the authoriser still cannot write through the read path.
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
			return errors.New("sqlite driver does not support online backups")
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
func configureStorageConn(_ context.Context, conn *sql.Conn) error {
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

// withConn runs fn on the cached write pool under the shared restore lock.
func (s *storageServer) withConn(ctx context.Context, app string, fn func(*sql.Conn) error) error {
	s.opMu.RLock()
	defer s.opMu.RUnlock()

	return s.withConnUnlocked(ctx, app, fn)
}

// withConnUnlocked is withConn without the restore lock; callers that already hold it use this.
func (s *storageServer) withConnUnlocked(ctx context.Context, app string, fn func(*sql.Conn) error) (err error) {
	start := time.Now()

	defer func() {
		if s.metrics != nil {
			s.metrics.observeStorage(app, "write", err, time.Since(start))
		}
	}()

	db, err := s.dbFor(app)
	if err != nil {
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
	}
	defer conn.Close()

	if err := configureStorageConn(ctx, conn); err != nil {
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
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
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
	}
	defer conn.Close()

	if err := configureStorageConn(ctx, conn); err != nil {
		return fmt.Errorf("%w: %w", errStorageUnavailable, err)
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

// readStorageJSON reads a bounded JSON request body and writes the matching error when it cannot.
