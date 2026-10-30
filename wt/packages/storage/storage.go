// Package storage is a small client for the supervisor's per-app SQLite service. It reads WT_STORAGE_URL and WT_STORAGE_TOKEN from the environment.
package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

// ErrNoRows is returned by Row.Scan when the query returned no rows.
var ErrNoRows = errors.New("storage: no rows")

// Client talks to the storage service with a wasi:http transport in components.
type Client struct {
	baseURL, token string
	http           *http.Client
}

// Statement is one entry in a batch. All statements in a batch run in a single transaction.
type Statement struct {
	SQL  string
	Args []any
}

// Result describes the effect of an Exec or a batch statement.
type Result struct {
	RowsAffected, LastInsertID int64
}

// Rows is the result of a Query.
type Rows struct {
	Columns   []string
	Truncated bool
	rows      [][]any
	index     int
	err       error
	closed    bool
}

// Row is the result of a QueryRow.
type Row struct {
	rows *Rows
}

type request struct {
	SQL     string `json:"sql"`
	Params  []any  `json:"params,omitempty"`
	MaxRows int    `json:"max_rows,omitempty"`
}

type batchRequest struct {
	Statements []batchStatement `json:"statements"`
}

type batchStatement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

type response struct {
	Columns      []string           `json:"columns"`
	Rows         [][]jsontext.Value `json:"rows"`
	RowsAffected int64              `json:"rows_affected"`
	LastInsertID int64              `json:"last_insert_id"`
	Truncated    bool               `json:"truncated"`
}

type batchResponse struct {
	Results []response `json:"results"`
}

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

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.closed || r.err != nil || r.index >= len(r.rows) {
		return false
	}
	r.index++
	return true
}

// Close releases the rows. It exists so callers can use defer rows.Close().
func (r *Rows) Close() error {
	r.closed = true
	r.rows = nil
	return nil
}

// Err returns the error that stopped iteration, if any.
func (r *Rows) Err() error {
	return r.err
}

func encodeParam(arg any) any {
	switch value := arg.(type) {
	case []byte:
		return map[string]string{"$b64": base64.StdEncoding.EncodeToString(value)}
	case time.Time:
		return value.Format(time.RFC3339Nano)
	case bool:
		if value {
			return int64(1)
		}
		return int64(0)
	case int:
		return int64(value)
	case int8:
		return int64(value)
	case int16:
		return int64(value)
	case int32:
		return int64(value)
	case uint:
		return uint64(value)
	case uint8:
		return int64(value)
	case uint16:
		return int64(value)
	case uint32:
		return int64(value)
	default:
		return arg
	}
}

func encodeParams(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	params := make([]any, len(args))
	for i, arg := range args {
		params[i] = encodeParam(arg)
	}
	return params
}

// Exec runs one statement and returns the number of affected rows.
func (c *Client) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	var resp response
	if err := c.post(ctx, "/v1/exec", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: resp.RowsAffected, LastInsertID: resp.LastInsertID}, nil
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

func decodeValue(raw jsontext.Value) (any, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty value")
	}
	switch text[0] {
	case 'n':
		return nil, nil
	case 't':
		return true, nil
	case 'f':
		return false, nil
	case '"':
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return value, nil
	case '{':
		var object map[string]string
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf("blob must be {\"$b64\": \"...\"}")
		}
		encoded, ok := object["$b64"]
		if !ok {
			return nil, fmt.Errorf("blob must be {\"$b64\": \"...\"}")
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		return value, nil
	default:
		if strings.ContainsAny(text, ".eE") {
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, err
			}
			return value, nil
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, err
		}
		return value, nil
	}
}

// Query runs one statement and returns all rows up to the server's row limit.
func (c *Client) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	var resp response
	if err := c.post(ctx, "/v1/query", request{SQL: query, Params: encodeParams(args)}, &resp); err != nil {
		return nil, err
	}
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

func int64Value(value any) (int64, error) {
	switch value := value.(type) {
	case int64:
		return value, nil
	case float64:
		if value != math.Trunc(value) || value < math.MinInt64 || value > math.MaxInt64 {
			return 0, fmt.Errorf("cannot convert %v to int64", value)
		}
		return int64(value), nil
	case bool:
		if value {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseInt(value, 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", value)
	}
}

func float64Value(value any) (float64, error) {
	switch value := value.(type) {
	case float64:
		return value, nil
	case int64:
		return float64(value), nil
	case string:
		return strconv.ParseFloat(value, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", value)
	}
}

func boolValue(value any) (bool, error) {
	switch value := value.(type) {
	case bool:
		return value, nil
	case int64:
		switch value {
		case 0:
			return false, nil
		case 1:
			return true, nil
		default:
			return false, fmt.Errorf("cannot convert %d to bool", value)
		}
	case string:
		return strconv.ParseBool(value)
	default:
		return false, fmt.Errorf("cannot convert %T to bool", value)
	}
}

func scanValue(dest, value any) error {
	if scanner, ok := dest.(interface{ Scan(any) error }); ok {
		return scanner.Scan(value)
	}
	switch dest := dest.(type) {
	case *any:
		*dest = value
	case *string:
		switch value := value.(type) {
		case string:
			*dest = value
		case []byte:
			*dest = string(value)
		case int64:
			*dest = strconv.FormatInt(value, 10)
		case float64:
			*dest = strconv.FormatFloat(value, 'g', -1, 64)
		default:
			return fmt.Errorf("cannot scan %T into *string", value)
		}
	case *[]byte:
		switch value := value.(type) {
		case []byte:
			*dest = value
		case string:
			*dest = []byte(value)
		default:
			return fmt.Errorf("cannot scan %T into *[]byte", value)
		}
	case *int64:
		value, err := int64Value(value)
		if err != nil {
			return err
		}
		*dest = value
	case *int:
		value, err := int64Value(value)
		if err != nil {
			return err
		}
		if int64(int(value)) != value {
			return fmt.Errorf("integer %d overflows int", value)
		}
		*dest = int(value)
	case *float64:
		value, err := float64Value(value)
		if err != nil {
			return err
		}
		*dest = value
	case *bool:
		value, err := boolValue(value)
		if err != nil {
			return err
		}
		*dest = value
	case *time.Time:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("cannot scan %T into *time.Time", value)
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return err
		}
		*dest = parsed
	default:
		return fmt.Errorf("cannot scan into %T", dest)
	}
	return nil
}

// Scan copies the current row into dest. It supports *string, *[]byte, *int64, *int, *float64, *bool, *time.Time, *any and anything with a Scan(any) error method.
func (r *Rows) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.closed {
		return fmt.Errorf("storage: rows are closed")
	}
	if r.index == 0 || r.index > len(r.rows) {
		return ErrNoRows
	}
	if len(dest) != len(r.Columns) {
		return fmt.Errorf("storage: scan %d destinations into %d columns", len(dest), len(r.Columns))
	}
	row := r.rows[r.index-1]
	for i, target := range dest {
		if err := scanValue(target, row[i]); err != nil {
			return fmt.Errorf("storage: column %q: %w", r.Columns[i], err)
		}
	}
	return nil
}

// Scan copies the first row into dest, or returns ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.rows == nil {
		return ErrNoRows
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Close()
}
