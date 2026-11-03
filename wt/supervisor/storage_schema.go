package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
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

type storageSchema struct {
	Version    int                      `json:"version"`
	Tables     []storageTable           `json:"tables"`
	Views      []storageView            `json:"views"`
	Migrations []storageMigrationRecord `json:"migrations"`
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

	rows, err := conn.QueryContext(ctx, "select version, checksum, applied_at from "+quoteSQLiteIdentifier(storageMigrationsTable)+" order by version") //nolint:gosec // The table name is a quoted identifier.
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
