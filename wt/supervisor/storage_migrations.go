package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	maxStorageMigrationVersion = 1_000_000_000
	// storageMigrationsTable is reserved for the supervisor's migration bookkeeping. Apps cannot read it through the schema listing or write it through SQL.
	storageMigrationsTable = "wt_migrations"
)

type storageMigrationRecord struct {
	Version   int    `json:"version"`
	Checksum  string `json:"checksum"`
	AppliedAt string `json:"appliedAt"`
}

type storageMigrationRequest struct {
	Version    int                `json:"version"`
	Statements []storageStatement `json:"statements,omitempty"`
}

type storageMigrationResponse struct {
	Version int  `json:"version"`
	Applied bool `json:"applied"`
}

// storageMigrationChecksum hashes the version, each statement's SQL and its raw JSON parameters. Length prefixes keep statement boundaries unambiguous.
func storageMigrationChecksum(version int, statements []storageStatement) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version:%d\n", version)

	_, _ = fmt.Fprintf(hash, "statements:%d\n", len(statements))
	for _, statement := range statements {
		_, _ = fmt.Fprintf(hash, "sql:%d:", len(statement.SQL))
		_, _ = hash.Write([]byte(statement.SQL))

		_, _ = fmt.Fprintf(hash, "\nparams:%d\n", len(statement.Params))
		for _, param := range statement.Params {
			_, _ = fmt.Fprintf(hash, "param:%d:", len(param))
			_, _ = hash.Write(param)
			_, _ = hash.Write([]byte{'\n'})
		}
	}

	return hex.EncodeToString(hash.Sum(nil))
}

// ensureStorageMigrationsTable creates the reserved bookkeeping table inside the migration transaction, before any app statement runs.
func ensureStorageMigrationsTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `create table if not exists wt_migrations (version integer primary key, checksum text not null, applied_at text not null)`)
	return err
}

// storedMigrationChecksum returns the checksum recorded for a version. ok is false when the version has no bookkeeping row.
func storedMigrationChecksum(ctx context.Context, tx *sql.Tx, version int) (checksum string, ok bool, err error) {
	err = tx.QueryRowContext(ctx, "select checksum from "+quoteSQLiteIdentifier(storageMigrationsTable)+" where version = ?", version).Scan(&checksum)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return checksum, true, nil
	}
}

// prepareMigration validates the version and statements and computes the migration checksum.
func prepareMigration(version int, statements []storageStatement) ([][]any, string, error) {
	if version < 1 || version > maxStorageMigrationVersion {
		return nil, "", fmt.Errorf("%w: version must be between 1 and %d", errStorageInvalid, maxStorageMigrationVersion)
	}

	params, err := decodeStorageStatements(statements, false)
	if err != nil {
		return nil, "", err
	}

	return params, storageMigrationChecksum(version, statements), nil
}

// applyMigration runs one version in a transaction and reports whether it was newly applied.
func applyMigration(ctx context.Context, conn *sql.Conn, version int, statements []storageStatement, params [][]any, checksum string) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := ensureStorageMigrationsTable(ctx, tx); err != nil {
		return false, err
	}

	var current int
	if err := tx.QueryRowContext(ctx, "pragma user_version").Scan(&current); err != nil {
		return false, err
	}

	if current >= version {
		stored, ok, err := storedMigrationChecksum(ctx, tx, version)
		if err != nil {
			return false, err
		}

		if !ok {
			return false, fmt.Errorf("%w: version %d has no bookkeeping row", errStorageMigrationChecksum, version)
		}

		if stored != checksum {
			return false, fmt.Errorf("%w: version %d was applied with a different checksum", errStorageMigrationChecksum, version)
		}

		return false, nil
	}

	unregister, err := requireStorageAuthorizer(conn, false)
	if err != nil {
		return false, err
	}
	defer func() { _ = unregister() }()

	for i, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.SQL, params[i]...); err != nil {
			return false, fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	// The version bump and checksum are host bookkeeping, not app SQL, so they run without the app authorizer.
	if err := unregister(); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx,
		"insert into "+quoteSQLiteIdentifier(storageMigrationsTable)+" (version, checksum, applied_at) values (?, ?, ?)", //nolint:gosec // The table name is a quoted identifier.
		version, checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", version)); err != nil {
		return false, err
	}

	return true, tx.Commit()
}

// migrate applies statements and records a new user_version and checksum in one transaction. A version at or below the current one is a no-op when its checksum matches; a missing or edited migration fails instead of silently doing nothing.
func (s *storageServer) migrate(ctx context.Context, app string, version int, statements []storageStatement) (storageMigrationResponse, error) {
	params, checksum, err := prepareMigration(version, statements)
	if err != nil {
		return storageMigrationResponse{}, err
	}

	applied := false

	err = s.withConn(ctx, app, func(conn *sql.Conn) error {
		var err error

		applied, err = applyMigration(ctx, conn, version, statements, params, checksum)

		return err
	})
	if err != nil {
		return storageMigrationResponse{}, err
	}

	return storageMigrationResponse{Version: version, Applied: applied}, nil
}
