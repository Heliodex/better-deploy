package storage

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type (
	// Client talks to the storage service with a wasi:http transport in components.
	Client struct {
		baseURL, token string
		http *http.Client
	}

	// Column is one column in a Table.
	Column struct {
		Name string `json:"name"`
		Type string `json:"type"`
		NotNull bool `json:"notNull"`
		Default *string `json:"default,omitempty"`
		PrimaryKey int `json:"primaryKey,omitempty"`
	}

	// Error is a typed storage service error. Status is the HTTP status, Code is the stable server code such as constraint or busy, and the message is also kept in the original string field on the wire.
	Error struct {
		Status int
		Code, Message string
		SQLiteCode int
		Constraint string
	}

	// ForeignKey is one outgoing foreign key in a Table. To is nil when SQLite reports a NULL target column.
	ForeignKey struct {
		ID int `json:"id"`
		Seq int `json:"seq"`
		Table string `json:"table"`
		From string `json:"from"`
		To *string `json:"to"`
		OnUpdate string `json:"onUpdate"`
		OnDelete string `json:"onDelete"`
		Match string `json:"match"`
	}

	// Index is one index in a Table. A nil entry in Columns is an expression or rowid column, which SQLite reports with a NULL name.
	Index struct {
		Name string `json:"name"`
		Unique bool `json:"unique"`
		Origin string `json:"origin"`
		Partial bool `json:"partial"`
		Columns []*string `json:"columns"`
	}

	// MigrationRecord is one applied migration and its deterministic checksum.
	MigrationRecord struct {
		Version int `json:"version"`
		Checksum string `json:"checksum"`
		AppliedAt string `json:"appliedAt"`
	}

	// Page is an offset page of a result: up to Limit rows starting at Offset.
	//
	// This is offset pagination, not keyset pagination. A concurrent insert or delete can shift rows between pages, so give the query a stable ORDER BY. For a consistent snapshot, page inside a transaction or build keyset predicates on top of QueryPage.
	Page struct {
		Limit, Offset int
	}

	// QueryOptions tunes one query. MaxRows caps the rows the server returns; zero uses the server's default.
	QueryOptions struct {
		MaxRows int
	}

	// Result describes the effect of an Exec or a batch statement.
	Result struct {
		RowsAffected, LastInsertID int64
	}

	// Rows is the result of a Query.
	Rows struct {
		Columns []string
		Truncated bool
		rows [][]any
		index int
		page Page
		err error
		closed bool
	}

	// Row is the result of a QueryRow.
	Row struct {
		rows *Rows
	}

	// SchemaOptions tunes schema introspection. Stats adds row counts and page sizes, which require scanning the database.
	SchemaOptions struct {
		Stats bool
	}

	// Statement is one entry in a batch. All statements in a batch run in a single transaction.
	Statement struct {
		SQL string
		Args []any
	}

	// Table is one table in a Schema.
	Table struct {
		Name string `json:"name"`
		Columns []Column `json:"columns"`
		Indexes []Index `json:"indexes"`
		ForeignKeys []ForeignKey `json:"foreignKeys"`
		Rows *int64 `json:"rows,omitempty"`
		Bytes *int64 `json:"bytes,omitempty"`
	}

	// TransactionResult is one statement's result. Read statements fill Columns and Rows; writes fill RowsAffected and LastInsertID.
	TransactionResult struct {
		Columns []string
		Rows [][]any
		RowsAffected, LastInsertID int64
		Truncated bool
	}

	// TransactionStatement is one entry in a Transaction. Set Query to return its rows instead of write counts.
	TransactionStatement struct {
		SQL string
		Args []any
		Query bool
	}

	// View is one view in a Schema, with its columns where SQLite can describe them.
	View struct {
		Name string `json:"name"`
		SQL string `json:"sql"`
		Columns []Column `json:"columns"`
	}

	// Schema describes the database's migration version, tables, views and applied migration checksums.
	Schema struct {
		Version int `json:"version"`
		Tables []Table `json:"tables"`
		Views []View `json:"views"`
		Migrations []MigrationRecord `json:"migrations"`
	}

	batchStatement struct {
		SQL string `json:"sql"`
		Params []any `json:"params,omitempty"`
	}

	batchRequest struct {
		Statements []batchStatement `json:"statements"`
	}

	errorResponse struct {
		Error string `json:"error"`
		Code string `json:"code"`
		SQLiteCode int `json:"sqlite_code"`
		Constraint string `json:"constraint"`
	}

	migrationRequest struct {
		Version int `json:"version"`
		Statements []batchStatement `json:"statements,omitempty"`
	}

	migrationResponse struct {
		Version int `json:"version"`
		Applied bool `json:"applied"`
	}

	request struct {
		SQL string `json:"sql"`
		Params []any `json:"params,omitempty"`
		MaxRows int `json:"max_rows,omitempty"`
	}

	response struct {
		Columns []string `json:"columns"`
		Rows [][]jsontext.Value `json:"rows"`
		RowsAffected int64 `json:"rows_affected"`
		LastInsertID int64 `json:"last_insert_id"`
		Truncated bool `json:"truncated"`
	}

	batchResponse struct {
		Results []response `json:"results"`
	}

	sessionCloseResponse struct {
		Committed bool `json:"committed"`
		RolledBack bool `json:"rolled_back"`
	}

	sessionResponse struct {
		ID string `json:"id"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	transactionResponse struct {
		Results []response `json:"results"`
	}

	transactionStatement struct {
		SQL string `json:"sql"`
		Params []any `json:"params,omitempty"`
		Query bool `json:"query,omitempty"`
	}

	transactionRequest struct {
		Statements []transactionStatement `json:"statements"`
	}
)

// ErrNoRows is returned by Row.Scan when the query returned no rows.
var ErrNoRows = errors.New("storage: no rows")

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("storage: %s: %s", e.Code, e.Message)
	}

	return fmt.Sprintf("storage: HTTP %d: %s", e.Status, e.Message)
}
