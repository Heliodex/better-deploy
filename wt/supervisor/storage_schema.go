package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	maxStorageMigrationVersion = 1_000_000_000
	// storageMigrationsTable is reserved for the supervisor's migration bookkeeping. Apps cannot read it through the schema listing or write it through SQL.
	storageMigrationsTable = "wt_migrations"
)

type storageColumn struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	NotNull    bool    `json:"notNull"`
	Default    *string `json:"default,omitempty"`
	PrimaryKey int     `json:"primaryKey,omitempty"`
}

// storageIndexColumn is one key column in index order. A nil name is an expression or rowid column, which SQLite reports with a NULL name.
type storageIndexColumn = *string

type storageIndex struct {
	Name    string               `json:"name"`
	Unique  bool                 `json:"unique"`
	Origin  string               `json:"origin"`
	Partial bool                 `json:"partial"`
	Columns []storageIndexColumn `json:"columns"`
}

type storageForeignKey struct {
	ID       int     `json:"id"`
	Seq      int     `json:"seq"`
	Table    string  `json:"table"`
	From     string  `json:"from"`
	To       *string `json:"to"`
	OnUpdate string  `json:"onUpdate"`
	OnDelete string  `json:"onDelete"`
	Match    string  `json:"match"`
}

type storageTable struct {
	Name        string              `json:"name"`
	Columns     []storageColumn     `json:"columns"`
	Indexes     []storageIndex      `json:"indexes"`
	ForeignKeys []storageForeignKey `json:"foreignKeys"`
	Rows        *int64              `json:"rows,omitempty"`
	Bytes       *int64              `json:"bytes,omitempty"`
}

type storageView struct {
	Name    string          `json:"name"`
	SQL     string          `json:"sql"`
	Columns []storageColumn `json:"columns"`
}

type storageMigrationRecord struct {
	Version   int    `json:"version"`
	Checksum  string `json:"checksum"`
	AppliedAt string `json:"appliedAt"`
}

type storageSchema struct {
	Version    int                      `json:"version"`
	Tables     []storageTable           `json:"tables"`
	Views      []storageView            `json:"views"`
	Migrations []storageMigrationRecord `json:"migrations"`
}

type storageMigrationRequest struct {
	Version    int                `json:"version"`
	Statements []storageStatement `json:"statements,omitempty"`
}

type storageMigrationResponse struct {
	Version int  `json:"version"`
	Applied bool `json:"applied"`
}

// isStorageMigrationsTable reports whether name is the supervisor's reserved migration table. SQLite identifiers are case-insensitive.
func isStorageMigrationsTable(name string) bool {
	return strings.EqualFold(name, storageMigrationsTable)
}

// isSQLiteInternalTable reports whether a table name belongs to SQLite or the migration bookkeeping, so it stays out of app-facing listings.
func isSQLiteInternalTable(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "sqlite_") || isStorageMigrationsTable(name)
}

// statsRequested reports whether a schema or database overview request opted into the expensive row and size statistics.
func statsRequested(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stats"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// schemaTableColumns reads one table's columns through SQLite's table_xinfo pragma.
func schemaTableColumns(ctx context.Context, conn *sql.Conn, table string) ([]storageColumn, error) {
	rows, err := conn.QueryContext(ctx, "pragma table_xinfo("+quoteSQLiteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := []storageColumn{}
	for rows.Next() {
		var (
			cid     int
			name    string
			kind    string
			notNull int
			def     sql.NullString
			pk      int
			hidden  int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &def, &pk, &hidden); err != nil {
			return nil, err
		}
		if hidden != 0 {
			continue
		}
		column := storageColumn{Name: name, Type: kind, NotNull: notNull != 0, PrimaryKey: pk}
		if def.Valid {
			column.Default = &def.String
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

// schemaTableIndexes reads an index's order-preserving key columns. Expression indexes report a NULL column name, which stays nil in JSON.
func schemaTableIndexes(ctx context.Context, conn *sql.Conn, table string) ([]storageIndex, error) {
	rows, err := conn.QueryContext(ctx, "pragma index_list("+quoteSQLiteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	var (
		indexes []storageIndex
		names   []string
	)
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			closeErr := rows.Close() //nolint:sqlclosecheck // Rows are closed explicitly so the close error can be returned.
			return nil, errors.Join(err, closeErr)
		}
		indexes = append(indexes, storageIndex{Name: name, Unique: unique != 0, Origin: origin, Partial: partial != 0, Columns: []storageIndexColumn{}})
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, name := range names {
		columnRows, err := conn.QueryContext(ctx, "pragma index_info("+quoteSQLiteIdentifier(name)+")")
		if err != nil {
			return nil, err
		}
		for columnRows.Next() {
			var (
				seqno int
				cid   int
				value sql.NullString
			)
			if err := columnRows.Scan(&seqno, &cid, &value); err != nil {
				closeErr := columnRows.Close() //nolint:sqlclosecheck // Rows are closed explicitly so the close error can be returned.
				return nil, errors.Join(err, closeErr)
			}
			var column *string
			if value.Valid {
				name := value.String
				column = &name
			}
			indexes[i].Columns = append(indexes[i].Columns, column)
		}
		if err := columnRows.Close(); err != nil {
			return nil, err
		}
		if err := columnRows.Err(); err != nil {
			return nil, err
		}
	}
	return indexes, nil
}

// schemaTableForeignKeys reads the table's outgoing foreign keys, keeping a NULL target column as null.
func schemaTableForeignKeys(ctx context.Context, conn *sql.Conn, table string) ([]storageForeignKey, error) {
	rows, err := conn.QueryContext(ctx, "pragma foreign_key_list("+quoteSQLiteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	foreignKeys := []storageForeignKey{}
	for rows.Next() {
		var (
			id       int
			seq      int
			refTable string
			from     sql.NullString
			to       sql.NullString
			onUpdate string
			onDelete string
			match    string
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, err
		}
		foreignKey := storageForeignKey{
			ID:       id,
			Seq:      seq,
			Table:    refTable,
			From:     from.String,
			OnUpdate: onUpdate,
			OnDelete: onDelete,
			Match:    match,
		}
		if to.Valid {
			target := to.String
			foreignKey.To = &target
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys, rows.Err()
}

// tableStats counts rows and, when dbstat is available, sums the table's pages. A missing dbstat module leaves bytes at zero instead of failing the request.
func tableStats(ctx context.Context, conn *sql.Conn, table string) (int64, int64, error) {
	var rows int64
	if err := conn.QueryRowContext(ctx, "select count(*) from "+quoteSQLiteIdentifier(table)).Scan(&rows); err != nil {
		return 0, 0, err
	}
	var bytes int64
	if err := conn.QueryRowContext(ctx, "select coalesce(sum(pgsize), 0) from dbstat where name = ?", table).Scan(&bytes); err != nil {
		bytes = 0
	}
	return rows, bytes, nil
}

// schemaViews lists views and their columns where SQLite can describe them.
func schemaViews(ctx context.Context, conn *sql.Conn) ([]storageView, error) {
	rows, err := conn.QueryContext(ctx, `select name, sql from sqlite_master where type = 'view' and lower(substr(name, 1, 7)) <> 'sqlite_' order by name`)
	if err != nil {
		return nil, err
	}
	var (
		views []storageView
		names []string
	)
	for rows.Next() {
		var (
			name string
			ddl  sql.NullString
		)
		if err := rows.Scan(&name, &ddl); err != nil {
			closeErr := rows.Close() //nolint:sqlclosecheck // Rows are closed explicitly so the close error can be returned.
			return nil, errors.Join(err, closeErr)
		}
		views = append(views, storageView{Name: name, SQL: ddl.String, Columns: []storageColumn{}})
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, name := range names {
		columns, err := schemaTableColumns(ctx, conn, name)
		if err != nil {
			// Some views cannot be introspected, for example a view over a dropped table. Keep the view and leave its columns empty.
			continue
		}
		views[i].Columns = columns
	}
	return views, nil
}

// schemaMigrations reads the checksummed migration bookkeeping, which stays hidden from the table list.
func schemaMigrations(ctx context.Context, conn *sql.Conn) ([]storageMigrationRecord, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, `select count(*) from sqlite_master where type = 'table' and lower(name) = ?`, storageMigrationsTable).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return []storageMigrationRecord{}, nil
	}
	rows, err := conn.QueryContext(ctx, "select version, checksum, applied_at from "+quoteSQLiteIdentifier(storageMigrationsTable)+" order by version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	migrations := []storageMigrationRecord{}
	for rows.Next() {
		var record storageMigrationRecord
		if err := rows.Scan(&record.Version, &record.Checksum, &record.AppliedAt); err != nil {
			return nil, err
		}
		migrations = append(migrations, record)
	}
	return migrations, rows.Err()
}

// schema returns the database's user_version, tables with their indexes and foreign keys, views and migration checksums, so an app can inspect what its migrations produced.
func (s *storageServer) schema(ctx context.Context, app string, stats bool) (storageSchema, error) {
	result := storageSchema{
		Tables:     []storageTable{},
		Views:      []storageView{},
		Migrations: []storageMigrationRecord{},
	}
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, "pragma user_version").Scan(&result.Version); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `select name from sqlite_master where type = 'table' and lower(substr(name, 1, 7)) <> 'sqlite_' and lower(name) <> ? order by name`, storageMigrationsTable)
		if err != nil {
			return err
		}
		names := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				closeErr := rows.Close() //nolint:sqlclosecheck // Rows are closed explicitly so the close error can be returned.
				return errors.Join(err, closeErr)
			}
			names = append(names, name)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range names {
			columns, err := schemaTableColumns(ctx, conn, name)
			if err != nil {
				return err
			}
			indexes, err := schemaTableIndexes(ctx, conn, name)
			if err != nil {
				return err
			}
			foreignKeys, err := schemaTableForeignKeys(ctx, conn, name)
			if err != nil {
				return err
			}
			table := storageTable{Name: name, Columns: columns, Indexes: indexes, ForeignKeys: foreignKeys}
			if stats {
				rowCount, bytes, err := tableStats(ctx, conn, name)
				if err != nil {
					return err
				}
				table.Rows, table.Bytes = &rowCount, &bytes
			}
			result.Tables = append(result.Tables, table)
		}
		if result.Views, err = schemaViews(ctx, conn); err != nil {
			return err
		}
		if result.Migrations, err = schemaMigrations(ctx, conn); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *storageServer) schemaVersion(ctx context.Context, app string) (int, error) {
	var version int
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "pragma user_version").Scan(&version)
	})
	return version, err
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
		"insert into "+quoteSQLiteIdentifier(storageMigrationsTable)+" (version, checksum, applied_at) values (?, ?, ?)",
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

// readSchema serves the app's schema overview, including optional statistics.
func (s *storageServer) readSchema(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()
	schema, err := s.schema(ctx, app, statsRequested(r))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schema)
}

// runMigration applies one user version with the host-wide migration timeout.
func (s *storageServer) runMigration(w http.ResponseWriter, r *http.Request, app string) {
	var req storageMigrationRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	timeout := s.migrationTimeout
	if timeout <= 0 {
		timeout = defaultStorageMigrationTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	result, err := s.migrate(ctx, app, req.Version, req.Statements)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
