package storage

import (
	"encoding/json/jsontext"
	"errors"
	"net/http"
)

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

// TransactionStatement is one entry in a Transaction. Set Query to return its rows instead of write counts.
type TransactionStatement struct {
	SQL   string
	Args  []any
	Query bool
}

// TransactionResult is one statement's result. Read statements fill Columns and Rows; writes fill RowsAffected and LastInsertID.
type TransactionResult struct {
	Columns                    []string
	Rows                       [][]any
	RowsAffected, LastInsertID int64
	Truncated                  bool
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

// Schema describes the database's migration version and its tables.
type Schema struct {
	Version int     `json:"version"`
	Tables  []Table `json:"tables"`
}

// Table is one table in a Schema.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// Column is one column in a Table.
type Column struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	NotNull    bool    `json:"notNull"`
	Default    *string `json:"default,omitempty"`
	PrimaryKey int     `json:"primaryKey,omitempty"`
}

type transactionStatement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
	Query  bool   `json:"query,omitempty"`
}

type transactionRequest struct {
	Statements []transactionStatement `json:"statements"`
}

type transactionResponse struct {
	Results []response `json:"results"`
}

type migrationRequest struct {
	Version    int              `json:"version"`
	Statements []batchStatement `json:"statements,omitempty"`
}

type migrationResponse struct {
	Version int  `json:"version"`
	Applied bool `json:"applied"`
}
