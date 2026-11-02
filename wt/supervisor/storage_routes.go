package main

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

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

// exec runs one write statement and records its touched rows in the change log.
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

// logBatchChanges turns the batch statements into change-log entries.
func (s *storageServer) logBatchChanges(app string, r *http.Request, req storageStatementsRequest, results []storageResult, starts, ends []int, collector *rowChangeCollector) {
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
}

// batch runs a list of write statements in one transaction.
func (s *storageServer) batch(w http.ResponseWriter, r *http.Request, app string) {
	var req storageStatementsRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	for i, statement := range req.Statements {
		if statement.Query {
			writeStorageError(w, fmt.Errorf("%w: batch statement %d cannot query", errStorageInvalid, i+1))
			return
		}
	}
	params, ok := validateStorageStatements(w, req.Statements, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	results, starts, ends, collector, err := s.executeTransaction(ctx, app, req, params)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.logBatchChanges(app, r, req, results, starts, ends, collector)
	writeJSON(w, http.StatusOK, storageStatementsResponse{Results: results})
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

// query runs one statement on the query-only pool.
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
	// Publish traffic is part of the event bus and does not consume a query slot.
	if r.URL.Path != "/v1/publish" {
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
	}
	s.storageRoutes().serve(w, r, app)
}
