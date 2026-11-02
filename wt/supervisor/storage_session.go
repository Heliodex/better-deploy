package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	errSessionNotFound = errors.New("storage session not found")
	errSessionClosed   = errors.New("storage session is closed")
)

// storageSession pins one connection and an open transaction across several requests, so apps can read, decide and write atomically.
type storageSession struct {
	id, app    string
	conn       *sql.Conn
	tx         *sql.Tx
	collector  *rowChangeCollector
	hook       bool
	authorizer bool
	writes     []storageSessionWrite

	mu        sync.Mutex
	closed    bool
	expiresAt time.Time // protected by storageServer.sessionMu
}

type storageSessionWrite struct {
	statement storageTransactionStatement
	result    storageResult
	start     int
	end       int
}

type storageSessionResponse struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type storageSessionCloseResponse struct {
	Committed  bool `json:"committed"`
	RolledBack bool `json:"rolled_back"`
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// pauseSessions blocks new sessions while a database file is replaced, then closeSessionsForPaused rolls back the open ones.
func (s *storageServer) pauseSessions() {
	s.sessionMu.Lock()
	s.sessionsPaused++
	s.sessionMu.Unlock()
}

func (s *storageServer) resumeSessions() {
	s.sessionMu.Lock()
	s.sessionsPaused--
	if s.sessionsPaused <= 0 {
		s.sessionsPaused = 0
		s.sessionCond.Broadcast()
	}
	s.sessionMu.Unlock()
}

func (s *storageServer) closeSessionsForPaused(app string) {
	s.sessionMu.Lock()
	var closing []*storageSession
	for id, session := range s.sessions {
		if app != "" && session.app != app {
			continue
		}
		delete(s.sessions, id)
		closing = append(closing, session)
	}
	s.sessionMu.Unlock()
	for _, session := range closing {
		_ = s.finishSession(session, false, context.Background())
	}
}

func (s *storageServer) closeSessionsFor(app string) {
	s.pauseSessions()
	defer s.resumeSessions()
	s.closeSessionsForPaused(app)
}

func (s *storageServer) sessionLoop() {
	defer close(s.sessionDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.sessionStop:
			return
		case now := <-ticker.C:
			s.expireSessions(now)
		}
	}
}

func (s *storageServer) expireSessions(now time.Time) {
	s.sessionMu.Lock()
	var expired []*storageSession
	for id, session := range s.sessions {
		if now.Before(session.expiresAt) {
			continue
		}
		delete(s.sessions, id)
		expired = append(expired, session)
	}
	s.sessionMu.Unlock()
	for _, session := range expired {
		_ = s.finishSession(session, false, context.Background())
	}
}

func (s *storageServer) withSession(id, app string, fn func(*storageSession) error) error {
	now := time.Now()
	s.sessionMu.Lock()
	session := s.sessions[id]
	if session == nil || session.app != app {
		s.sessionMu.Unlock()
		return errSessionNotFound
	}
	if !now.Before(session.expiresAt) {
		delete(s.sessions, id)
		s.sessionMu.Unlock()
		_ = s.finishSession(session, false, context.Background())
		return errSessionNotFound
	}
	session.expiresAt = now.Add(storageSessionLease)
	s.sessionMu.Unlock()

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return errSessionClosed
	}
	return fn(session)
}

func (s *storageServer) closeSession(id, app string, commit bool, ctx context.Context) error {
	s.sessionMu.Lock()
	session := s.sessions[id]
	if session == nil || session.app != app {
		s.sessionMu.Unlock()
		return errSessionNotFound
	}
	expired := !time.Now().Before(session.expiresAt)
	delete(s.sessions, id)
	s.sessionMu.Unlock()
	if expired {
		_ = s.finishSession(session, false, context.Background())
		return errSessionNotFound
	}
	return s.finishSession(session, commit, ctx)
}

// finishSession commits or rolls back the pinned transaction, logs committed writes and releases the connection.
func (s *storageServer) finishSession(session *storageSession, commit bool, ctx context.Context) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil
	}
	session.closed = true

	var err error
	if session.authorizer {
		if clearErr := unregisterStorageAuthorizer(session.conn); clearErr != nil {
			log.Printf("storage %s: clear session authorizer: %v", session.app, clearErr)
			commit = false
			err = clearErr
		}
	}
	if commit {
		if err = session.tx.Commit(); err != nil {
			_ = session.tx.Rollback()
		} else {
			session.collector.resolveColumns(context.WithoutCancel(ctx), session.conn)
			s.logChanges(session.app, session.changes())
		}
	} else {
		_ = session.tx.Rollback()
	}
	if session.hook {
		if hookErr := unregisterRowHook(session.conn); hookErr != nil {
			log.Printf("storage %s: clear session hook: %v", session.app, hookErr)
		}
	}
	_ = session.conn.Close()
	return err
}

func (session *storageSession) changes() []storageChange {
	out := make([]storageChange, 0, len(session.writes))
	for _, write := range session.writes {
		var rows []rowChange
		if write.start <= write.end && write.end <= len(session.collector.rows) {
			rows = session.collector.rows[write.start:write.end]
		}
		out = append(out, storageChange{
			Time:          time.Now().UTC(),
			SQL:           write.statement.SQL,
			Params:        write.statement.Params,
			RowsAffected:  write.result.RowsAffected,
			Rows:          rows,
			RowsTruncated: session.collector.omitted > 0,
		})
	}
	return out
}

func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errSessionClosed):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeStorageError(w, err)
	}
}

// openSession opens one pinned write connection and starts its transaction.
func (s *storageServer) openSession(w http.ResponseWriter, ctx context.Context, app string) (*storageSession, bool) {
	db, err := s.dbFor(app)
	if err != nil {
		writeStorageError(w, fmt.Errorf("%w: %v", errStorageUnavailable, err))
		return nil, false
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		writeStorageError(w, fmt.Errorf("%w: %v", errStorageUnavailable, err))
		return nil, false
	}
	if err := configureStorageConn(ctx, conn); err != nil {
		_ = conn.Close()
		writeStorageError(w, fmt.Errorf("%w: %v", errStorageUnavailable, err))
		return nil, false
	}
	collector := &rowChangeCollector{}
	hook := true
	if err := registerRowHook(conn, collector); err != nil {
		log.Printf("storage %s: session pre-update hook: %v", app, err)
		hook = false
	}
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		if hook {
			_ = unregisterRowHook(conn)
		}
		_ = conn.Close()
		writeStorageError(w, err)
		return nil, false
	}
	if _, err := requireStorageAuthorizer(conn, false); err != nil {
		_ = tx.Rollback()
		if hook {
			_ = unregisterRowHook(conn)
		}
		_ = conn.Close()
		writeStorageError(w, err)
		return nil, false
	}
	id, err := newSessionID()
	if err != nil {
		_ = unregisterStorageAuthorizer(conn)
		_ = tx.Rollback()
		if hook {
			_ = unregisterRowHook(conn)
		}
		_ = conn.Close()
		writeError(w, http.StatusInternalServerError, "create session id")
		return nil, false
	}
	return &storageSession{
		id:         id,
		app:        app,
		conn:       conn,
		tx:         tx,
		collector:  collector,
		hook:       hook,
		authorizer: true,
		expiresAt:  time.Now().Add(storageSessionLease),
	}, true
}

// registerSession enforces the per-app lease limit and adds the session to the live map.
func (s *storageServer) registerSession(w http.ResponseWriter, session *storageSession) bool {
	s.sessionMu.Lock()
	for s.sessionsPaused > 0 && !s.sessionClosed {
		s.sessionCond.Wait()
	}
	if s.sessionClosed {
		s.sessionMu.Unlock()
		_ = s.finishSession(session, false, context.Background())
		writeError(w, http.StatusServiceUnavailable, "storage is shutting down")
		return false
	}
	appSessions := 0
	for _, other := range s.sessions {
		if other.app == session.app {
			appSessions++
		}
	}
	if appSessions >= maxStorageSessions {
		s.sessionMu.Unlock()
		_ = s.finishSession(session, false, context.Background())
		writeError(w, http.StatusTooManyRequests, "too many storage sessions")
		return false
	}
	s.sessions[session.id] = session
	s.sessionMu.Unlock()
	return true
}

func (s *storageServer) createSession(w http.ResponseWriter, r *http.Request, app string) {
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	session, ok := s.openSession(w, ctx, app)
	if !ok {
		return
	}
	if !s.registerSession(w, session) {
		return
	}
	writeJSON(w, http.StatusOK, storageSessionResponse{ID: session.id, ExpiresAt: session.expiresAt})
}

func (s *storageServer) sessionQuery(w http.ResponseWriter, r *http.Request, app, id string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeStorageError(w, err)
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	maxRows := req.MaxRows
	if maxRows <= 0 || maxRows > maxStorageRows {
		maxRows = maxStorageRows
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	result := storageResult{Columns: []string{}, Rows: [][]any{}}
	err = s.withSession(id, app, func(session *storageSession) error {
		start := len(session.collector.rows)
		rows, err := session.tx.QueryContext(ctx, req.SQL, params...)
		if err != nil {
			session.collector.rows = session.collector.rows[:start]
			return err
		}
		defer rows.Close()
		result.Columns, result.Rows, result.Truncated, err = readStorageRows(rows, maxRows)
		if err != nil {
			session.collector.rows = session.collector.rows[:start]
			return err
		}
		if end := len(session.collector.rows); end > start {
			session.writes = append(session.writes, storageSessionWrite{
				statement: storageTransactionStatement{SQL: req.SQL, Params: req.Params, Query: true},
				start:     start,
				end:       end,
			})
		}
		return nil
	})
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *storageServer) sessionExec(w http.ResponseWriter, r *http.Request, app, id string) {
	var req storageRequest
	if !readStorageJSON(w, r, &req) {
		return
	}
	if err := validateStorageSQL(req.SQL); err != nil {
		writeStorageError(w, err)
		return
	}
	params, err := decodeStorageParams(req.Params)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()

	var result storageResult
	err = s.withSession(id, app, func(session *storageSession) error {
		start := len(session.collector.rows)
		res, err := session.tx.ExecContext(ctx, req.SQL, params...)
		if err != nil {
			session.collector.rows = session.collector.rows[:start]
			return err
		}
		end := len(session.collector.rows)
		result.RowsAffected, _ = res.RowsAffected()
		result.LastInsertID, _ = res.LastInsertId()
		session.writes = append(session.writes, storageSessionWrite{
			statement: storageTransactionStatement{SQL: req.SQL, Params: req.Params},
			result:    result,
			start:     start,
			end:       end,
		})
		return nil
	})
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *storageServer) closeSessionRequest(w http.ResponseWriter, r *http.Request, app, id string, commit bool) {
	ctx, cancel := context.WithTimeout(r.Context(), storageTimeout)
	defer cancel()
	if err := s.closeSession(id, app, commit, ctx); err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, storageSessionCloseResponse{Committed: commit, RolledBack: !commit})
}

// sessionAction splits /v1/sessions/<id>/<action> and reports whether the path names a session operation.
func sessionAction(path string) (string, string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1/sessions/")
	if !ok || rest == "" {
		return "", "", false
	}
	id, action, ok := strings.Cut(rest, "/")
	if !ok || id == "" || action == "" || strings.Contains(action, "/") {
		return "", "", false
	}
	switch action {
	case "query", "exec", "commit", "rollback":
		return id, action, true
	default:
		return "", "", false
	}
}
