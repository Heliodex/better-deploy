package storage

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxResponseBytes = 16 << 20

// New creates a client using the supplied transport. A wasi:http transport is used in components; tests can pass a normal http.Client.
func New(httpClient *http.Client) (*Client, error) {
	baseURL := strings.TrimSuffix(os.Getenv("WT_STORAGE_URL"), "/")
	token := os.Getenv("WT_STORAGE_TOKEN")
	if baseURL == "" || token == "" {
		return nil, fmt.Errorf("WT_STORAGE_URL and WT_STORAGE_TOKEN must be set")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, http: httpClient}, nil
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("storage: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("storage: %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), out); err != nil {
		return fmt.Errorf("storage: decode response: %w", err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("storage: %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), out); err != nil {
		return fmt.Errorf("storage: decode response: %w", err)
	}
	return nil
}

func decodeResponseRows(resp response) ([][]any, error) {
	rows := make([][]any, len(resp.Rows))
	for i, rawRow := range resp.Rows {
		if len(rawRow) != len(resp.Columns) {
			return nil, fmt.Errorf("storage: row %d has %d values for %d columns", i+1, len(rawRow), len(resp.Columns))
		}
		row := make([]any, len(rawRow))
		for j, raw := range rawRow {
			value, err := decodeValue(raw)
			if err != nil {
				return nil, fmt.Errorf("storage: row %d column %q: %w", i+1, resp.Columns[j], err)
			}
			row[j] = value
		}
		rows[i] = row
	}
	return rows, nil
}

// Query runs one statement and returns all rows up to the server's row limit.
func (c *Client) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	var resp response
	if err := c.post(ctx, "/v1/query", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return nil, err
	}
	rows, err := decodeResponseRows(resp)
	if err != nil {
		return nil, err
	}
	return &Rows{Columns: resp.Columns, Truncated: resp.Truncated, rows: rows}, nil
}

// QueryRow runs a query expected to return at most one row.
func (c *Client) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := c.Query(ctx, query, args...)
	if err != nil {
		return &Row{rows: &Rows{err: err}}
	}
	return &Row{rows: rows}
}

// Exec runs one statement and returns the number of affected rows.
func (c *Client) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	var resp response
	if err := c.post(ctx, "/v1/exec", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: resp.RowsAffected, LastInsertID: resp.LastInsertID}, nil
}

// Transaction runs statements in one transaction and returns each result, including rows for statements marked Query. A failure rolls back every statement.
func (c *Client) Transaction(ctx context.Context, statements ...TransactionStatement) ([]TransactionResult, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	req := transactionRequest{Statements: make([]transactionStatement, len(statements))}
	for i, statement := range statements {
		req.Statements[i] = transactionStatement{SQL: statement.SQL, Params: encodeParams(statement.Args), Query: statement.Query}
	}
	var resp transactionResponse
	if err := c.post(ctx, "/v1/transaction", req, &resp); err != nil {
		return nil, err
	}
	results := make([]TransactionResult, len(resp.Results))
	for i, result := range resp.Results {
		rows, err := decodeResponseRows(result)
		if err != nil {
			return nil, err
		}
		results[i] = TransactionResult{
			Columns:      result.Columns,
			Rows:         rows,
			RowsAffected: result.RowsAffected,
			LastInsertID: result.LastInsertID,
			Truncated:    result.Truncated,
		}
	}
	return results, nil
}

// Schema returns the database's user_version and table definitions.
func (c *Client) Schema(ctx context.Context) (Schema, error) {
	var schema Schema
	if err := c.get(ctx, "/v1/schema", &schema); err != nil {
		return Schema{}, err
	}
	return schema, nil
}

// Migrate applies statements and records version in one transaction. Applying a version at or below the current one is a no-op, so older code can start against a newer database while migrations stay expand/contract.
func (c *Client) Migrate(ctx context.Context, version int, statements ...Statement) error {
	req := migrationRequest{Version: version, Statements: make([]batchStatement, len(statements))}
	for i, statement := range statements {
		req.Statements[i] = batchStatement{SQL: statement.SQL, Params: encodeParams(statement.Args)}
	}
	var resp migrationResponse
	return c.post(ctx, "/v1/migrate", req, &resp)
}

// Batch runs statements in one transaction. A failing statement rolls the whole batch back.
func (c *Client) Batch(ctx context.Context, statements ...Statement) ([]Result, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	req := batchRequest{Statements: make([]batchStatement, len(statements))}
	for i, statement := range statements {
		req.Statements[i] = batchStatement{SQL: statement.SQL, Params: encodeParams(statement.Args)}
	}
	var resp batchResponse
	if err := c.post(ctx, "/v1/batch", req, &resp); err != nil {
		return nil, err
	}
	results := make([]Result, len(resp.Results))
	for i, result := range resp.Results {
		results[i] = Result{RowsAffected: result.RowsAffected, LastInsertID: result.LastInsertID}
	}
	return results, nil
}
