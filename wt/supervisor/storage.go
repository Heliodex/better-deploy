package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxStorageValue = 1 << 20
	maxStorageKeys  = 1000
)

// storageServer is a tiny per-app key-value API backed by SQLite. It listens on
// loopback and authenticates apps with a token, so app processes never touch
// the database files directly.
type storageServer struct {
	dir      string
	baseURL  string
	listener net.Listener
	server   *http.Server

	mu     sync.Mutex
	tokens map[string]string // token -> app name
	dbs    map[string]*sql.DB
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
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
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
	if _, err := db.Exec(`pragma busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage busy timeout: %w", err)
	}
	if _, err := db.Exec(`pragma journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage journal mode: %w", err)
	}
	if _, err := db.Exec(`create table if not exists kv (
		key text primary key,
		value blob not null,
		updated_at integer not null
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage schema: %w", err)
	}
	s.dbs[app] = db
	return db, nil
}

func (s *storageServer) handle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFor(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wt-storage"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.URL.Path == "/v1/kv" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.list(w, r, app)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/kv/") {
		http.NotFound(w, r)
		return
	}

	key, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/kv/"))
	if err != nil || key == "" {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.get(w, r, app, key)
	case http.MethodPut:
		s.put(w, r, app, key)
	case http.MethodDelete:
		s.delete(w, app, key)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *storageServer) get(w http.ResponseWriter, _ *http.Request, app, key string) {
	db, err := s.dbFor(app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var value []byte
	err = db.QueryRow(`select value from kv where key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, nil)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/octet-stream")
	_, _ = w.Write(value)
}

func (s *storageServer) put(w http.ResponseWriter, r *http.Request, app, key string) {
	value, err := io.ReadAll(io.LimitReader(r.Body, maxStorageValue+1))
	if err != nil {
		http.Error(w, "read value", http.StatusBadRequest)
		return
	}
	if len(value) > maxStorageValue {
		http.Error(w, "value too large", http.StatusRequestEntityTooLarge)
		return
	}
	db, err := s.dbFor(app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := db.Exec(`insert into kv (key, value, updated_at) values (?, ?, ?)
		on conflict(key) do update set value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *storageServer) delete(w http.ResponseWriter, app, key string) {
	db, err := s.dbFor(app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := db.Exec(`delete from kv where key = ?`, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *storageServer) list(w http.ResponseWriter, r *http.Request, app string) {
	db, err := s.dbFor(app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	query := `select key from kv`
	args := []any{}
	if prefix != "" {
		query += ` where instr(key, ?) = 1`
		args = append(args, prefix)
	}
	query += ` order by key limit ?`
	args = append(args, maxStorageKeys)

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string][]string{"keys": keys})
}
