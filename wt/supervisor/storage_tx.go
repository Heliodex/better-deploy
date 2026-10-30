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
func (s *storageServer) transaction(w http.ResponseWriter, r *http.Request, app string) {
	var req storageTransactionRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if len(req.Statements) == 0 {
		writeError(w, http.StatusBadRequest, "at least one statement is required")
		return
	}
	if len(req.Statements) > maxStorageStatements {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d statements are allowed", maxStorageStatements))
		return
	}
	params := make([][]any, len(req.Statements))
	for i, statement := range req.Statements {
		if err := validateStorageSQL(statement.SQL); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("statement %d: %v", i+1, err))
			return
		}
		decoded, err := decodeStorageParams(statement.Params)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("statement %d: %v", i+1, err))
			return
		}
		params[i] = decoded
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

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

		for i, statement := range req.Statements {
			starts[i] = len(changes.rows)
			if statement.Query {
				rows, err := tx.QueryContext(ctx, statement.SQL, params[i]...)
				if err != nil {
					return fmt.Errorf("statement %d: %w", i+1, err)
				}
				columns, encoded, truncated, err := readStorageRows(rows, maxStorageRows)
				closeErr := rows.Close()
				if err != nil {
					return fmt.Errorf("statement %d: %w", i+1, err)
				}
				if closeErr != nil {
					return fmt.Errorf("statement %d: %w", i+1, closeErr)
				}
				results[i] = storageResult{Columns: columns, Rows: encoded, Truncated: truncated}
			} else {
				res, err := tx.ExecContext(ctx, statement.SQL, params[i]...)
				if err != nil {
					return fmt.Errorf("statement %d: %w", i+1, err)
				}
				affected, _ := res.RowsAffected()
				lastID, _ := res.LastInsertId()
				results[i] = storageResult{RowsAffected: affected, LastInsertID: lastID}
			}
			ends[i] = len(changes.rows)
		}
		return tx.Commit()
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}

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
	s.logChanges(app, changes)
	writeJSON(w, http.StatusOK, storageTransactionResponse{Results: results})
}
