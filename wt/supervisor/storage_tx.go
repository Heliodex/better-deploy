package main

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"time"
)

type storageTransactionStatement struct {
	SQL    string           `json:"sql"`
	Params []jsontext.Value `json:"params,omitempty"`
	// Query marks statements whose rows should be returned; otherwise the result carries rows_affected and last_insert_id.
	Query bool `json:"query,omitempty"`
}

type storageTransactionRequest struct {
	Statements []storageTransactionStatement `json:"statements"`
}

type storageTransactionResponse struct {
	Results []storageResult `json:"results"`
}

// transaction runs statements in one SQLite transaction and can return rows from reads as well as write counts.
// readTransactionRequest parses and validates one batch request.
func readTransactionRequest(w http.ResponseWriter, r *http.Request) (storageTransactionRequest, [][]any, bool) {
	var req storageTransactionRequest
	if !readStorageJSON(w, r, &req) {
		return req, nil, false
	}
	if len(req.Statements) == 0 {
		writeStorageError(w, fmt.Errorf("%w: at least one statement is required", errStorageInvalid))
		return req, nil, false
	}
	if len(req.Statements) > maxStorageStatements {
		writeStorageError(w, fmt.Errorf("%w: at most %d statements are allowed", errStorageInvalid, maxStorageStatements))
		return req, nil, false
	}
	params := make([][]any, len(req.Statements))
	for i, statement := range req.Statements {
		if err := validateStorageSQL(statement.SQL); err != nil {
			writeStorageError(w, fmt.Errorf("statement %d: %w", i+1, err))
			return req, nil, false
		}
		decoded, err := decodeStorageParams(statement.Params)
		if err != nil {
			writeStorageError(w, fmt.Errorf("statement %d: %w", i+1, err))
			return req, nil, false
		}
		params[i] = decoded
	}
	return req, params, true
}

// executeStatement runs one statement inside the transaction and records its row-change range.
func executeStatement(ctx context.Context, tx *sql.Tx, statement storageTransactionStatement, params []any, changes *rowChangeCollector, index int) (storageResult, int, int, error) {
	start := len(changes.rows)
	if statement.Query {
		rows, err := tx.QueryContext(ctx, statement.SQL, params...)
		if err != nil {
			return storageResult{}, start, start, fmt.Errorf("statement %d: %w", index+1, err)
		}
		columns, encoded, truncated, err := readStorageRows(rows, maxStorageRows)
		closeErr := rows.Close()
		if err != nil {
			return storageResult{}, start, start, fmt.Errorf("statement %d: %w", index+1, err)
		}
		if closeErr != nil {
			return storageResult{}, start, start, fmt.Errorf("statement %d: %w", index+1, closeErr)
		}
		return storageResult{Columns: columns, Rows: encoded, Truncated: truncated}, start, len(changes.rows), nil
	}
	res, err := tx.ExecContext(ctx, statement.SQL, params...)
	if err != nil {
		return storageResult{}, start, start, fmt.Errorf("statement %d: %w", index+1, err)
	}
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	return storageResult{RowsAffected: affected, LastInsertID: lastID}, start, len(changes.rows), nil
}

// executeTransaction runs every statement in one transaction and returns the per-statement results and row ranges.
func (s *storageServer) executeTransaction(ctx context.Context, app string, req storageTransactionRequest, params [][]any) ([]storageResult, []int, []int, *rowChangeCollector, error) {
	results := make([]storageResult, len(req.Statements))
	starts := make([]int, len(req.Statements))
	ends := make([]int, len(req.Statements))
	var collector *rowChangeCollector
	err := s.withWriteConn(ctx, app, func(conn *sql.Conn, changes *rowChangeCollector) error {
		collector = changes
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
			result, start, end, err := executeStatement(ctx, tx, statement, params[i], changes, i)
			if err != nil {
				return err
			}
			results[i], starts[i], ends[i] = result, start, end
		}
		if err := unregister(); err != nil {
			return err
		}
		return tx.Commit()
	})
	return results, starts, ends, collector, err
}

// transactionChanges turns the non-query statements into change-log entries.
func transactionChanges(req storageTransactionRequest, results []storageResult, starts, ends []int, collector *rowChangeCollector) []storageChange {
	changes := []storageChange{}
	for i, statement := range req.Statements {
		if statement.Query {
			continue
		}
		var rows []rowChange
		if collector != nil && starts[i] <= ends[i] {
			rows = collector.rows[starts[i]:ends[i]]
		}
		changes = append(changes, storageChange{
			Time:          time.Now().UTC(),
			SQL:           statement.SQL,
			Params:        statement.Params,
			RowsAffected:  results[i].RowsAffected,
			Rows:          rows,
			RowsTruncated: collector != nil && collector.omitted > 0,
		})
	}
	return changes
}

func (s *storageServer) transaction(w http.ResponseWriter, r *http.Request, app string) {
	req, params, ok := readTransactionRequest(w, r)
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
	s.logChanges(app, transactionChanges(req, results, starts, ends, collector))
	writeJSON(w, http.StatusOK, storageTransactionResponse{Results: results})
}
