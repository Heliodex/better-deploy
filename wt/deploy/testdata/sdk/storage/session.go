package storage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var errTxDone = errors.New("storage: transaction is closed")

// Tx is a storage session pinned to one connection and one open SQLite transaction.
type Tx struct {
	client    *Client
	id        string
	expiresAt time.Time
	done      bool
}

// BeginTx opens a transaction session. Call Commit or Rollback, or use WithTx to run a callback and commit it.
func (c *Client) BeginTx(ctx context.Context) (*Tx, error) {
	var resp sessionResponse
	if err := c.post(ctx, "/v1/sessions", struct{}{}, &resp); err != nil {
		return nil, err
	}
	if resp.ID == "" {
		return nil, fmt.Errorf("storage: session response has no id")
	}
	return &Tx{client: c, id: resp.ID, expiresAt: resp.ExpiresAt}, nil
}

// WithTx runs fn in one storage session and commits if it returns nil. A returned error rolls the transaction back.
func (c *Client) WithTx(ctx context.Context, fn func(*Tx) error) (err error) {
	tx, err := c.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if !tx.done {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ExpiresAt is when the server will roll back an idle session.
func (tx *Tx) ExpiresAt() time.Time {
	return tx.expiresAt
}

// Query runs one statement inside the session and returns its rows.
func (tx *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if tx.done {
		return nil, errTxDone
	}
	var resp response
	if err := tx.client.post(ctx, "/v1/sessions/"+tx.id+"/query", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return nil, err
	}
	rows, err := decodeResponseRows(resp)
	if err != nil {
		return nil, err
	}
	return &Rows{Columns: resp.Columns, Truncated: resp.Truncated, rows: rows}, nil
}

// QueryRow runs a query expected to return at most one row.
func (tx *Tx) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return &Row{rows: &Rows{err: err}}
	}
	return &Row{rows: rows}
}

// Exec runs one statement inside the session and returns its write counts.
func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	if tx.done {
		return Result{}, errTxDone
	}
	var resp response
	if err := tx.client.post(ctx, "/v1/sessions/"+tx.id+"/exec", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: resp.RowsAffected, LastInsertID: resp.LastInsertID}, nil
}

// Commit writes the session's changes and releases its connection.
func (tx *Tx) Commit(ctx context.Context) error {
	if tx.done {
		return errTxDone
	}
	var resp sessionCloseResponse
	if err := tx.client.post(ctx, "/v1/sessions/"+tx.id+"/commit", struct{}{}, &resp); err != nil {
		return err
	}
	tx.done = true
	return nil
}

// Rollback discards the session's changes and releases its connection.
func (tx *Tx) Rollback(ctx context.Context) error {
	if tx.done {
		return nil
	}
	tx.done = true
	var resp sessionCloseResponse
	return tx.client.post(ctx, "/v1/sessions/"+tx.id+"/rollback", struct{}{}, &resp)
}
