package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	maxStorageValue      = 1 << 20
	maxStorageBody       = 8 << 20
	maxStorageSQL        = 64 << 10
	maxStorageParams     = 128
	maxStorageRows       = 10_000
	maxStorageStatements = 64
	maxStorageColumns    = 256
	maxStorageResult     = 8 << 20
	storageTimeout       = 5 * time.Second
)

var errStorageUnavailable = errors.New("storage unavailable")

// storageServer is a tiny per-app SQLite API. It listens on loopback and authenticates apps with a token, so app processes never touch the database files directly.
type storageServer struct {
	dir, baseURL string
	listener     net.Listener
	server       *http.Server

	mu, changeMu sync.Mutex
	tokens       map[string]string // token -> app name
	dbs          map[string]*sql.DB
}

type storageRequest struct {
	SQL     string           `json:"sql"`
	Params  []jsontext.Value `json:"params,omitempty"`
	MaxRows int              `json:"max_rows,omitempty"`
}

type storageResult struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Truncated    bool     `json:"truncated"`
}

type storageStatement struct {
	SQL    string           `json:"sql"`
	Params []jsontext.Value `json:"params,omitempty"`
}

type storageBatchRequest struct {
	Statements []storageStatement `json:"statements"`
}

type storageBatchResponse struct {
	Results []storageResult `json:"results"`
}

func newStorageServer(dir, listen string) (*storageServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage dir: %w", err)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("storage listen: %w", err)
	}

	s := &storageServer{
		dir:     dir,
		baseURL: "http://" + listener.Addr().String(),
		tokens:  map[string]string{},
		dbs:     map[string]*sql.DB{},
	}
	s.listener = listener
	s.server = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("storage: %v", err)
		}
	}()
	return s, nil
}

func (s *storageServer) url() string {
	return s.baseURL
}

func (s *storageServer) close() {
	_ = s.server.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, db := range s.dbs {
		_ = db.Close()
	}
}

// tokenFor returns the app's stable token for this supervisor run.
func (s *storageServer) tokenFor(app string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, name := range s.tokens {
		if name == app {
			return token
		}
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	s.tokens[token] = app
	return token
}

func (s *storageServer) appFor(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), prefix)
	if !ok {
		return "", false
	}
	token = strings.TrimSpace(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.tokens[token]
	return app, ok
}

func (s *storageServer) dbFor(app string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.dbs[app]; db != nil {
		return db, nil
	}

	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("app storage dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "storage.db"))
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, pragma := range []string{
		`pragma busy_timeout = 5000`,
		`pragma journal_mode = WAL`,
		`pragma synchronous = NORMAL`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("storage pragma: %w", err)
		}
	}
	s.dbs[app] = db
	return db, nil
}

// databasePath is the app's live SQLite file, outside the app's /data preopen.
func (s *storageServer) databasePath(app string) string {
	return filepath.Join(s.dir, app, "storage.db")
}

// hasDatabase reports whether the app has used storage yet. Checking the file avoids creating an empty database just to inspect it.
func (s *storageServer) hasDatabase(app string) bool {
	info, err := os.Stat(s.databasePath(app))
	return err == nil && info.Mode().IsRegular()
}

// sqliteBackup is the slice of modernc.org/sqlite's connection API used by the online backup calls.
type sqliteBackup interface {
	NewBackup(dst string) (*sqlite.Backup, error)
	NewRestore(src string) (*sqlite.Backup, error)
}

// runSQLiteBackup drives SQLite's online backup API to completion on one connection.
func runSQLiteBackup(conn *sql.Conn, start func(sqliteBackup) (*sqlite.Backup, error)) error {
	return conn.Raw(func(driverConn any) error {
		driver, ok := driverConn.(sqliteBackup)
		if !ok {
			return fmt.Errorf("sqlite driver does not support online backups")
		}
		backup, err := start(driver)
		if err != nil {
			return err
		}
		for {
			more, err := backup.Step(-1)
			if err != nil {
				_ = backup.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return backup.Finish()
	})
}

// checkBackupDatabase rejects a truncated or corrupt snapshot before it is restored over live data.
func checkBackupDatabase(path string) error {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), backupCheckTimeout)
	defer cancel()
	var result string
	if err := db.QueryRowContext(ctx, "pragma quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check: %s", result)
	}
	return nil
}

// configureStorageConn applies per-connection safety limits. ATTACH is disabled at the SQLite level, which also blocks VACUUM INTO and therefore arbitrary file writes from SQL.
func configureStorageConn(ctx context.Context, conn *sql.Conn) error {
	for _, pragma := range []string{
		`pragma busy_timeout = 5000`,
		`pragma foreign_keys = ON`,
		`pragma trusted_schema = OFF`,
		`pragma temp_store = MEMORY`,
	} {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	for _, limit := range []struct {
		id, value int
	}{
		{sqlite3.SQLITE_LIMIT_ATTACHED, 0},
		{sqlite3.SQLITE_LIMIT_SQL_LENGTH, maxStorageSQL},
		{sqlite3.SQLITE_LIMIT_LENGTH, maxStorageValue},
		{sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, maxStorageParams},
		{sqlite3.SQLITE_LIMIT_COLUMN, maxStorageColumns},
		{sqlite3.SQLITE_LIMIT_EXPR_DEPTH, 100},
		{sqlite3.SQLITE_LIMIT_COMPOUND_SELECT, 20},
	} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *storageServer) withConn(ctx context.Context, app string, fn func(*sql.Conn) error) error {
	db, err := s.dbFor(app)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	defer conn.Close()
	if err := configureStorageConn(ctx, conn); err != nil {
		return fmt.Errorf("%w: %v", errStorageUnavailable, err)
	}
	return fn(conn)
}

// backupTo writes a consistent snapshot to dst while the app keeps reading and writing.
func (s *storageServer) backupTo(ctx context.Context, app, dst string) error {
	return s.withConn(ctx, app, func(conn *sql.Conn) error {
		return runSQLiteBackup(conn, func(driver sqliteBackup) (*sqlite.Backup, error) {
			return driver.NewBackup(dst)
		})
	})
}

// restoreFrom copies a snapshot over the live database. The storage pool has one connection, so no query can be in flight while the copy runs.
func (s *storageServer) restoreFrom(ctx context.Context, app, src string) error {
	return s.withConn(ctx, app, func(conn *sql.Conn) error {
		return runSQLiteBackup(conn, func(driver sqliteBackup) (*sqlite.Backup, error) {
			return driver.NewRestore(src)
		})
	})
}

func (s *storageServer) handleSQL(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fn(w, r)
}

func readStorageJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxStorageBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return false
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errStorageUnavailable):
		writeError(w, http.StatusInternalServerError, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "storage query timed out")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// readStorageQuotedWord returns the contents of a double-quoted or backtick-quoted identifier. SQLite allows function names such as load_extension to be quoted, so these contents must be checked too.
func readStorageQuotedWord(query string, i int, quote byte) (string, int) {
	var word strings.Builder
	i++
	for i < len(query) {
		if query[i] != quote {
			word.WriteByte(query[i])
			i++
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			word.WriteByte(quote)
			i += 2
			continue
		}
		return word.String(), i + 1
	}
	return word.String(), i
}

func readStorageBracketWord(query string, i int) (string, int) {
	i++
	start := i
	for i < len(query) && query[i] != ']' {
		i++
	}
	word := query[start:i]
	if i < len(query) {
		i++
	}
	return word, i
}

func skipStorageString(query string, i int) int {
	i++
	for i < len(query) {
		if query[i] != '\'' {
			i++
			continue
		}
		if i+1 < len(query) && query[i+1] == '\'' {
			i += 2
			continue
		}
		return i + 1
	}
	return i
}

func skipStorageLineComment(query string, i int) int {
	i += 2
	for i < len(query) && query[i] != '\n' {
		i++
	}
	return i
}

func skipStorageBlockComment(query string, i int) int {
	i += 2
	for i+1 < len(query) {
		if query[i] == '*' && query[i+1] == '/' {
			return i + 2
		}
		i++
	}
	return len(query)
}

func isStorageWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func storageSQLWords(query string) []string {
	words := []string{}
	for i := 0; i < len(query); {
		switch c := query[i]; {
		case c == '\'':
			i = skipStorageString(query, i)
		case c == '"' || c == '`':
			word, next := readStorageQuotedWord(query, i, c)
			if word != "" {
				words = append(words, strings.ToLower(word))
			}
			i = next
		case c == '[':
			word, next := readStorageBracketWord(query, i)
			if word != "" {
				words = append(words, strings.ToLower(word))
			}
			i = next
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			i = skipStorageLineComment(query, i)
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			i = skipStorageBlockComment(query, i)
		case isStorageWordByte(c):
			start := i
			for i < len(query) && isStorageWordByte(query[i]) {
				i++
			}
			words = append(words, strings.ToLower(query[start:i]))
		default:
			i++
		}
	}
	return words
}

// validateStorageSQL rejects the few SQLite features that can reach outside the app's database file. ATTACH and VACUUM INTO are also blocked by SQLITE_LIMIT_ATTACHED; the word check keeps the error understandable and covers older SQLite builds.
func validateStorageSQL(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("sql must not be empty")
	}
	if len(query) > maxStorageSQL {
		return fmt.Errorf("sql is too long (max %d bytes)", maxStorageSQL)
	}
	words := storageSQLWords(query)
	if len(words) > 0 {
		switch words[0] {
		case "begin", "commit", "rollback", "end", "savepoint", "release":
			return fmt.Errorf("transaction control statements are not allowed; use /v1/batch")
		}
	}
	for _, word := range words {
		switch word {
		case "attach", "detach", "vacuum", "pragma", "load_extension", "readfile", "writefile", "writable_schema":
			return fmt.Errorf("sql uses %q, which is not allowed", word)
		}
	}
	return nil
}

func decodeStorageValue(raw jsontext.Value) (any, error) {
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
			return nil, fmt.Errorf("object parameters must be {\"$b64\": \"...\"}")
		}
		encoded, ok := object["$b64"]
		if !ok {
			return nil, fmt.Errorf("object parameters must be {\"$b64\": \"...\"}")
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

func decodeStorageParams(raw []jsontext.Value) ([]any, error) {
	if len(raw) > maxStorageParams {
		return nil, fmt.Errorf("too many parameters (max %d)", maxStorageParams)
	}
	params := make([]any, len(raw))
	for i, value := range raw {
		decoded, err := decodeStorageValue(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", i+1, err)
		}
		params[i] = decoded
	}
	return params, nil
}

func (s *storageServer) exec(w http.ResponseWriter, r *http.Request, app string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	var result storageResult
	var collector *rowChangeCollector
	err = s.withWriteConn(ctx, app, func(conn *sql.Conn, changes *rowChangeCollector) error {
		collector = changes
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
		SQL:           req.SQL,
		Params:        req.Params,
		RowsAffected:  result.RowsAffected,
		Rows:          rows,
		RowsTruncated: truncated,
	}})
	writeJSON(w, http.StatusOK, result)
}

func (s *storageServer) batch(w http.ResponseWriter, r *http.Request, app string) {
	var req storageBatchRequest
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

	results := make([]storageResult, 0, len(req.Statements))
	starts := make([]int, len(req.Statements))
	ends := make([]int, len(req.Statements))
	var collector *rowChangeCollector
	err := s.withWriteConn(ctx, app, func(conn *sql.Conn, rows *rowChangeCollector) error {
		collector = rows
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		for i, statement := range req.Statements {
			starts[i] = len(rows.rows)
			res, err := tx.ExecContext(ctx, statement.SQL, params[i]...)
			if err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
			ends[i] = len(rows.rows)
			affected, _ := res.RowsAffected()
			lastID, _ := res.LastInsertId()
			results = append(results, storageResult{RowsAffected: affected, LastInsertID: lastID})
		}
		return tx.Commit()
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
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
			SQL:           statement.SQL,
			Params:        statement.Params,
			RowsAffected:  results[i].RowsAffected,
			Rows:          rows,
			RowsTruncated: truncated,
		}
	}
	s.logChanges(app, changes)
	writeJSON(w, http.StatusOK, storageBatchResponse{Results: results})
}

func encodeStorageValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return map[string]string{"$b64": base64.StdEncoding.EncodeToString(value)}
	case time.Time:
		return value.Format(time.RFC3339Nano)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		return value
	default:
		return value
	}
}

func storageValueSize(value any) int {
	switch value := value.(type) {
	case string:
		return len(value)
	case []byte:
		return base64.StdEncoding.EncodedLen(len(value))
	case int64, float64:
		return 8
	case time.Time:
		return 32
	case nil:
		return 4
	default:
		return 8
	}
}

func (s *storageServer) query(w http.ResponseWriter, r *http.Request, app string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	maxRows := req.MaxRows
	if maxRows <= 0 || maxRows > maxStorageRows {
		maxRows = maxStorageRows
	}

	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	result := storageResult{Columns: []string{}, Rows: [][]any{}}
	err = s.withConn(ctx, app, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, req.SQL, params...)
		if err != nil {
			return err
		}
		defer rows.Close()

		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		if len(columns) > maxStorageColumns {
			return fmt.Errorf("query returns too many columns")
		}
		result.Columns = columns

		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		total := 0
		for rows.Next() {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			size := 0
			for _, value := range values {
				size += storageValueSize(value)
			}
			if total+size > maxStorageResult {
				result.Truncated = true
				break
			}
			total += size
			row := make([]any, len(values))
			for i, value := range values {
				row[i] = encodeStorageValue(value)
			}
			result.Rows = append(result.Rows, row)
		}
		return rows.Err()
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *storageServer) handle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFor(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wt-storage"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/v1/query":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.query(w, r, app)
		})
	case "/v1/exec":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.exec(w, r, app)
		})
	case "/v1/batch":
		s.handleSQL(w, r, func(w http.ResponseWriter, r *http.Request) {
			s.batch(w, r, app)
		})
	default:
		http.NotFound(w, r)
	}
}
