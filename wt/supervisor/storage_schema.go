package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
)

const maxStorageMigrationVersion = 1_000_000_000

type storageColumn struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	NotNull    bool    `json:"notNull"`
	Default    *string `json:"default,omitempty"`
	PrimaryKey int     `json:"primaryKey,omitempty"`
}

type storageTable struct {
	Name    string          `json:"name"`
	Columns []storageColumn `json:"columns"`
}

type storageSchema struct {
	Version int            `json:"version"`
	Tables  []storageTable `json:"tables"`
}

type storageMigrationRequest struct {
	Version    int                `json:"version"`
	Statements []storageStatement `json:"statements,omitempty"`
}

type storageMigrationResponse struct {
	Version int  `json:"version"`
	Applied bool `json:"applied"`
}

// tableColumns reads one table's columns through SQLite's table_xinfo pragma.
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

// schema returns the database's user_version and its tables, so an app can inspect what its migrations produced.
func (s *storageServer) schema(ctx context.Context, app string) (storageSchema, error) {
	result := storageSchema{Tables: []storageTable{}}
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, "pragma user_version").Scan(&result.Version); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `select name from sqlite_master where type = 'table' and substr(name, 1, 7) <> 'sqlite_' order by name`)
		if err != nil {
			return err
		}
		names := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
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
			result.Tables = append(result.Tables, storageTable{Name: name, Columns: columns})
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

// migrate applies statements and records a new user_version in one transaction. A version at or below the current one is a no-op, so an older blue or green process can start against a newer database.
func (s *storageServer) migrate(ctx context.Context, app string, version int, statements []storageStatement) (storageMigrationResponse, error) {
	if version < 1 || version > maxStorageMigrationVersion {
		return storageMigrationResponse{}, fmt.Errorf("version must be between 1 and %d", maxStorageMigrationVersion)
	}
	if len(statements) > maxStorageStatements {
		return storageMigrationResponse{}, fmt.Errorf("at most %d statements are allowed", maxStorageStatements)
	}
	params := make([][]any, len(statements))
	for i, statement := range statements {
		if err := validateStorageSQL(statement.SQL); err != nil {
			return storageMigrationResponse{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		decoded, err := decodeStorageParams(statement.Params)
		if err != nil {
			return storageMigrationResponse{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		params[i] = decoded
	}

	var current int
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if err := tx.QueryRowContext(ctx, "pragma user_version").Scan(&current); err != nil {
			return err
		}
		if current >= version {
			return nil
		}
		for i, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement.SQL, params[i]...); err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", version)); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return storageMigrationResponse{}, err
	}
	return storageMigrationResponse{Version: version, Applied: current < version}, nil
}

func (s *storageServer) readSchema(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()
	schema, err := s.schema(ctx, app)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schema)
}

func (s *storageServer) runMigration(w http.ResponseWriter, r *http.Request, app string) {
	var req storageMigrationRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()
	result, err := s.migrate(ctx, app, req.Version, req.Statements)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
