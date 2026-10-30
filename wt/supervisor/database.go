package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	Version  int      `json:"version"`
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

		columns, encoded, truncated, err := readStorageRows(rows, limit)
		if err != nil {
			return err
		}
		result.Columns = columns
		result.Rows = encoded
		result.Truncated = truncated
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
	version, err := s.storage.schemaVersion(r.Context(), name)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, databaseOverviewResponse{Database: true, Tables: tables, Version: version})
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

type databaseImportResponse struct {
	Imported bool `json:"imported"`
}

type databaseMaintenanceRequest struct {
	Op string `json:"op"`
}

type databaseMaintenanceResponse struct {
	Op     string `json:"op"`
	Result string `json:"result,omitempty"`
}

// handleDatabaseExport streams a consistent SQLite copy of the live database.
func (s *supervisor) handleDatabaseExport(w http.ResponseWriter, r *http.Request, name string) {
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
	temp, err := os.CreateTemp(s.cfg.stateDir, ".database-export-*.db")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create export file")
		return
	}
	path := temp.Name()
	_ = temp.Close()
	defer os.Remove(path)

	ctx, cancel := context.WithTimeout(r.Context(), storageMaintenanceTimeout)
	defer cancel()
	if err := s.storage.backupTo(ctx, name, path); err != nil {
		writeStorageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".db"))
	http.ServeFile(w, r, path)
}

// handleDatabaseImport replaces the live database with an uploaded SQLite file after an integrity check.
func (s *supervisor) handleDatabaseImport(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.requireStorage(w, r, name) {
		return
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, maxBackupExtractBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse database upload: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("database")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing database field")
		return
	}
	defer file.Close()

	temp, err := os.CreateTemp(s.cfg.stateDir, ".database-import-*.db")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create import file")
		return
	}
	path := temp.Name()
	defer os.Remove(path)
	if _, err := io.Copy(temp, io.LimitReader(file, maxBackupExtractBytes)); err != nil {
		_ = temp.Close()
		writeError(w, http.StatusBadRequest, "read database: "+err.Error())
		return
	}
	if err := temp.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "close import file")
		return
	}
	if err := checkBackupDatabase(path); err != nil {
		writeError(w, http.StatusBadRequest, "not a valid SQLite database: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageMaintenanceTimeout)
	defer cancel()
	if err := s.storage.restoreFrom(ctx, name, path); err != nil {
		writeStorageError(w, err)
		return
	}
	log.Printf("database %s: imported %s", name, path)
	writeJSON(w, http.StatusOK, databaseImportResponse{Imported: true})
}

// handleDatabaseMaintenance runs vacuum, optimize, checkpoint or integrity_check on the live database.
func (s *supervisor) handleDatabaseMaintenance(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.requireStorage(w, r, name) {
		return
	}
	if !s.storage.hasDatabase(name) {
		writeError(w, http.StatusBadRequest, errNoStorageDatabase.Error())
		return
	}
	var req databaseMaintenanceRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	op := strings.ToLower(strings.TrimSpace(req.Op))
	ctx, cancel := context.WithTimeout(r.Context(), storageMaintenanceTimeout)
	defer cancel()
	result, err := s.storage.maintenance(ctx, name, op)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, databaseMaintenanceResponse{Op: op, Result: result})
}
