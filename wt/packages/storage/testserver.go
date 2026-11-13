//go:build !wasip1

package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// TestServer is an in-memory storage service for handler tests. It speaks the same HTTP wire format as the supervisor's storage service, backed by SQLite, and is excluded from wasm builds.
type TestServer struct {
	*httptest.Server
	Token string

	db *sql.DB

	mu         sync.Mutex
	sessions   map[string]*sql.Tx
	migrations int
	checksums  map[int]string
}

var testServerSequence atomic.Uint64

type testStatement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
	Query  bool   `json:"query,omitempty"`
}

type testRequest struct {
	SQL     string `json:"sql"`
	Params  []any  `json:"params,omitempty"`
	MaxRows int    `json:"max_rows,omitempty"`
}

type testStatements struct {
	Statements []testStatement `json:"statements"`
}

type testMigration struct {
	Version    int             `json:"version"`
	Statements []testStatement `json:"statements,omitempty"`
}

type testResponse struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowsAffected int64    `json:"rows_affected,omitempty"`
	LastInsertID int64    `json:"last_insert_id,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
}

type testResults struct {
	Results []testResponse `json:"results"`
}

// NewTestServer starts an isolated in-memory storage service. Call Close when the test finishes.
func NewTestServer() (*TestServer, error) {
	dsn := fmt.Sprintf("file:storage-test-%d?mode=memory&cache=shared", testServerSequence.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection keeps an in-memory database visible to sessions and serialises test traffic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	server := &TestServer{Token: "test-token", db: db, sessions: map[string]*sql.Tx{}, checksums: map[int]string{}}
	server.Server = httptest.NewServer(http.HandlerFunc(server.handle))
	return server, nil
}

// NewClient returns a storage client pointed at this server, without touching WT_STORAGE_* variables.
func (s *TestServer) NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: s.URL, token: s.Token, http: httpClient}
}

// Close stops the HTTP server and rolls back any open sessions.
func (s *TestServer) Close() {
	s.Server.Close()
	s.mu.Lock()
	for _, tx := range s.sessions {
		_ = tx.Rollback()
	}
	s.sessions = nil
	s.mu.Unlock()
	_ = s.db.Close()
}

// handle authenticates one storage request and dispatches the same endpoint shape as the supervisor.
func (s *TestServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeStorageTestError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	switch {
	case r.URL.Path == "/v1/query":
		var request testRequest
		if !readStorageTestJSON(w, r, &request) {
			return
		}
		s.respondQuery(w, r.Context(), s.db, request, false)
	case r.URL.Path == "/v1/write":
		var request testRequest
		if !readStorageTestJSON(w, r, &request) {
			return
		}
		s.respondQuery(w, r.Context(), s.db, request, true)
	case r.URL.Path == "/v1/exec":
		var request testRequest
		if !readStorageTestJSON(w, r, &request) {
			return
		}
		s.respondExec(w, r.Context(), s.db, testStatement{SQL: request.SQL, Params: request.Params})
	case r.URL.Path == "/v1/batch":
		s.handleBatch(w, r)
	case r.URL.Path == "/v1/transaction":
		s.handleTransaction(w, r)
	case r.URL.Path == "/v1/migrate":
		s.handleMigration(w, r)
	case r.URL.Path == "/v1/sessions":
		s.createSession(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		s.handleSession(w, r)
	default:
		http.NotFound(w, r)
	}
}

// respondQuery writes a query or returning-write response.
func (s *TestServer) respondQuery(w http.ResponseWriter, ctx context.Context, q testQueryer, request testRequest, write bool) {
	response, err := testQuery(ctx, q, request)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	if write && response.RowsAffected == 0 {
		response.RowsAffected = int64(len(response.Rows))
	}
	writeStorageTestJSON(w, http.StatusOK, response)
}

// respondExec writes one exec response.
func (s *TestServer) respondExec(w http.ResponseWriter, ctx context.Context, q testExecer, statement testStatement) {
	response, err := testExec(ctx, q, statement)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	writeStorageTestJSON(w, http.StatusOK, response)
}

// testQueryer is shared by *sql.DB and *sql.Tx.
type testQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// testExecer is shared by *sql.DB and *sql.Tx.
type testExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// testQuery runs one query and normalises row values for the storage wire format.
func testQuery(ctx context.Context, q testQueryer, request testRequest) (testResponse, error) {
	maxRows := request.MaxRows
	if maxRows <= 0 || maxRows > 1000 {
		maxRows = 1000
	}
	rows, err := q.QueryContext(ctx, request.SQL, testParams(request.Params)...)
	if err != nil {
		return testResponse{}, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return testResponse{}, err
	}
	columnTypes, _ := rows.ColumnTypes()
	out := make([][]any, 0, maxRows)
	truncated := false
	for rows.Next() {
		if len(out) >= maxRows {
			truncated = true
			break
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return testResponse{}, err
		}
		for i, value := range values {
			values[i] = testValue(value, columnTypes, i)
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		return testResponse{}, err
	}
	return testResponse{Columns: columns, Rows: out, Truncated: truncated}, nil
}

// testExec runs one write and reports its counts.
func testExec(ctx context.Context, q testExecer, statement testStatement) (testResponse, error) {
	result, err := q.ExecContext(ctx, statement.SQL, testParams(statement.Params)...)
	if err != nil {
		return testResponse{}, err
	}
	affected, _ := result.RowsAffected()
	lastID, _ := result.LastInsertId()
	return testResponse{RowsAffected: affected, LastInsertID: lastID}, nil
}

// testValue normalises one driver value to a JSON scalar or the blob envelope.
func testValue(value any, columnTypes []*sql.ColumnType, index int) any {
	switch value := value.(type) {
	case []byte:
		if index < len(columnTypes) && strings.Contains(strings.ToUpper(columnTypes[index].DatabaseTypeName()), "BLOB") {
			return map[string]string{"$b64": base64.StdEncoding.EncodeToString(value)}
		}
		return string(value)
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
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
		return value
	}
}

// testParams normalises JSON numbers and blob envelopes for database/sql.
func testParams(values []any) []any {
	out := make([]any, len(values))
	for i, value := range values {
		switch value := value.(type) {
		case float64:
			if value == math.Trunc(value) {
				out[i] = int64(value)
				continue
			}
			out[i] = value
		case map[string]any:
			if encoded, ok := value["$b64"].(string); ok {
				if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					out[i] = decoded
					continue
				}
			}
			out[i] = value
		default:
			out[i] = value
		}
	}
	return out
}

// handleBatch runs one transaction over every write statement.
func (s *TestServer) handleBatch(w http.ResponseWriter, r *http.Request) {
	var request testStatements
	if !readStorageTestJSON(w, r, &request) {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	results := make([]testResponse, len(request.Statements))
	for i, statement := range request.Statements {
		results[i], err = testExec(r.Context(), tx, statement)
		if err != nil {
			_ = tx.Rollback()
			writeStorageTestSQLError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	writeStorageTestJSON(w, http.StatusOK, testResults{Results: results})
}

// handleTransaction runs reads and writes in one transaction.
func (s *TestServer) handleTransaction(w http.ResponseWriter, r *http.Request) {
	var request testStatements
	if !readStorageTestJSON(w, r, &request) {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	results := make([]testResponse, len(request.Statements))
	for i, statement := range request.Statements {
		if statement.Query {
			results[i], err = testQuery(r.Context(), tx, testRequest{SQL: statement.SQL, Params: statement.Params})
		} else {
			results[i], err = testExec(r.Context(), tx, statement)
		}
		if err != nil {
			_ = tx.Rollback()
			writeStorageTestSQLError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	writeStorageTestJSON(w, http.StatusOK, testResults{Results: results})
}

// handleMigration records a version and runs its statements once.
func (s *TestServer) handleMigration(w http.ResponseWriter, r *http.Request) {
	var request testMigration
	if !readStorageTestJSON(w, r, &request) {
		return
	}
	checksum := testMigrationChecksum(request)
	s.mu.Lock()
	current := s.migrations
	stored, recorded := s.checksums[request.Version]
	s.mu.Unlock()
	if request.Version <= current {
		// Like the supervisor, an applied version must be resent unchanged. Versions applied before checksums were recorded here have none to compare.
		if recorded && stored != checksum {
			message := fmt.Sprintf("migration checksum mismatch: version %d was already applied with different statements; leave applied versions unchanged and put new statements in version %d", request.Version, current+1)
			writeStorageTestError(w, http.StatusConflict, message, "migration_checksum")
			return
		}
		writeStorageTestJSON(w, http.StatusOK, map[string]any{"version": request.Version, "applied": false})
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	for _, statement := range request.Statements {
		if _, err := testExec(r.Context(), tx, statement); err != nil {
			_ = tx.Rollback()
			writeStorageTestSQLError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	s.mu.Lock()
	s.migrations = request.Version
	s.checksums[request.Version] = checksum
	s.mu.Unlock()
	writeStorageTestJSON(w, http.StatusOK, map[string]any{"version": request.Version, "applied": true})
}

// testMigrationChecksum hashes a migration's statements so a re-sent version can be compared with the one applied.
func testMigrationChecksum(request testMigration) string {
	data, _ := json.Marshal(request.Statements)
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// createSession pins one transaction for WithTx.
func (s *TestServer) createSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		writeStorageTestSQLError(w, err)
		return
	}
	id := fmt.Sprintf("session-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.sessions[id] = tx
	s.mu.Unlock()
	writeStorageTestJSON(w, http.StatusOK, map[string]any{"id": id, "expiresAt": time.Now().Add(time.Minute).UTC()})
}

// handleSession routes one session operation.
func (s *TestServer) handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id, action, found := strings.Cut(rest, "/")
	s.mu.Lock()
	tx := s.sessions[id]
	s.mu.Unlock()
	if !found || tx == nil {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "query":
		var request testRequest
		if !readStorageTestJSON(w, r, &request) {
			return
		}
		response, err := testQuery(r.Context(), tx, request)
		if err != nil {
			writeStorageTestSQLError(w, err)
			return
		}
		writeStorageTestJSON(w, http.StatusOK, response)
	case "exec":
		var request testRequest
		if !readStorageTestJSON(w, r, &request) {
			return
		}
		response, err := testExec(r.Context(), tx, testStatement{SQL: request.SQL, Params: request.Params})
		if err != nil {
			writeStorageTestSQLError(w, err)
			return
		}
		writeStorageTestJSON(w, http.StatusOK, response)
	case "commit":
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		if err := tx.Commit(); err != nil {
			writeStorageTestSQLError(w, err)
			return
		}
		writeStorageTestJSON(w, http.StatusOK, map[string]bool{"committed": true})
	case "rollback":
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		if err := tx.Rollback(); err != nil {
			writeStorageTestSQLError(w, err)
			return
		}
		writeStorageTestJSON(w, http.StatusOK, map[string]bool{"rolled_back": true})
	default:
		http.NotFound(w, r)
	}
}

// readStorageTestJSON decodes a bounded JSON body and writes a 400 on failure.
func readStorageTestJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeStorageTestError(w, http.StatusBadRequest, err.Error(), "invalid")
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		writeStorageTestError(w, http.StatusBadRequest, err.Error(), "invalid")
		return false
	}
	return true
}

// writeStorageTestSQLError maps a driver error to the storage error shape.
func writeStorageTestSQLError(w http.ResponseWriter, err error) {
	message := err.Error()
	status, code := http.StatusInternalServerError, "internal"
	if strings.Contains(message, "UNIQUE constraint failed") {
		status, code = http.StatusConflict, "constraint"
	} else if strings.Contains(message, "no such table") || strings.Contains(message, "syntax error") {
		status, code = http.StatusBadRequest, "invalid"
	}
	writeStorageTestError(w, status, message, code)
}

// writeStorageTestError writes one storage error response.
func writeStorageTestError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := json.Marshal(map[string]string{"error": message, "code": code})
	_, _ = w.Write(append(data, '\n'))
}

// writeStorageTestJSON writes one storage success response.
func writeStorageTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := json.Marshal(value)
	_, _ = w.Write(append(data, '\n'))
}
