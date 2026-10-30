package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultDatabaseRows = 50
	maxDatabaseRows     = 500
)

var errDatabaseTableNotFound = errors.New("table not found")

type databaseOverviewResponse struct {
	Database bool     `json:"database"`
	Tables   []string `json:"tables"`
}

// databaseRowsResponse is one page of a table, with values encoded like the app-facing SQL service.
type databaseRowsResponse struct {
	Table     string   `json:"table"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Total     int      `json:"total"`
	Limit     int      `json:"limit"`
	Offset    int      `json:"offset"`
	Truncated bool     `json:"truncated"`
}

// databaseTables lists the app's user tables, excluding SQLite's internal tables.
func (s *storageServer) databaseTables(ctx context.Context, app string) ([]string, error) {
	tables := []string{}
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `select name from sqlite_master where type = 'table' and substr(name, 1, 7) <> 'sqlite_' order by name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			tables = append(tables, name)
		}
		return rows.Err()
	})
	return tables, err
}

// quoteSQLiteIdentifier quotes a table name for the few statements that cannot bind it as a parameter.
func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// databaseRows returns one page from a table. The table name is checked against sqlite_master before it is quoted into SQL, so the query parameter can never select anything other than an existing table in this app's database.
func (s *storageServer) databaseRows(ctx context.Context, app, table string, limit, offset int) (databaseRowsResponse, error) {
	result := databaseRowsResponse{
		Table:   table,
		Columns: []string{},
		Rows:    [][]any{},
		Limit:   limit,
		Offset:  offset,
	}
	err := s.withConn(ctx, app, func(conn *sql.Conn) error {
		var name string
		err := conn.QueryRowContext(ctx, `select name from sqlite_master where type = 'table' and name = ?`, table).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return errDatabaseTableNotFound
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(name, "sqlite_") {
			return errDatabaseTableNotFound
		}

		quoted := quoteSQLiteIdentifier(name)
		if err := conn.QueryRowContext(ctx, "select count(*) from "+quoted).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, "select * from "+quoted+" limit ? offset ?", limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()

		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		result.Columns = columns

		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		totalSize := 0
		for rows.Next() {
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			size := 0
			for _, value := range values {
				size += storageValueSize(value)
			}
			if totalSize+size > maxStorageResult {
				result.Truncated = true
				break
			}
			totalSize += size
			row := make([]any, len(values))
			for i, value := range values {
				row[i] = encodeStorageValue(value)
			}
			result.Rows = append(result.Rows, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if offset+len(result.Rows) < result.Total {
			result.Truncated = true
		}
		return nil
	})
	if err != nil {
		return databaseRowsResponse{}, err
	}
	return result, nil
}

// databaseRowsParam reads the page size and offset, writing 400 when either is out of range.
func databaseRowsParam(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit = defaultDatabaseRows
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maxDatabaseRows {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxDatabaseRows))
			return 0, 0, false
		}
		limit = parsed
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "offset must be 0 or more")
			return 0, 0, false
		}
		offset = parsed
	}
	return limit, offset, true
}

// requireStorage authenticates the request and checks the app exists and the storage service is running.
func (s *supervisor) requireStorage(w http.ResponseWriter, r *http.Request, name string) bool {
	if _, ok := s.requireApp(w, r, name); !ok {
		return false
	}
	if s.storage == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *supervisor) handleDatabase(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.requireStorage(w, r, name) {
		return
	}
	if !s.storage.hasDatabase(name) {
		writeJSON(w, http.StatusOK, databaseOverviewResponse{Tables: []string{}})
		return
	}
	tables, err := s.storage.databaseTables(r.Context(), name)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, databaseOverviewResponse{Database: true, Tables: tables})
}

func (s *supervisor) handleDatabaseRows(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.requireStorage(w, r, name) {
		return
	}
	if !s.storage.hasDatabase(name) {
		writeError(w, http.StatusBadRequest, errNoStorageDatabase.Error())
		return
	}
	table := strings.TrimSpace(r.URL.Query().Get("table"))
	if table == "" {
		writeError(w, http.StatusBadRequest, "table is required")
		return
	}
	limit, offset, ok := databaseRowsParam(w, r)
	if !ok {
		return
	}
	result, err := s.storage.databaseRows(r.Context(), name, table, limit, offset)
	if errors.Is(err, errDatabaseTableNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
