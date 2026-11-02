// Append-only audit trail for mutating admin actions.

package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	auditMaxBytes      = 16 << 20
	auditBodyBytes     = 2 << 10
	auditDefaultLimit  = 200
	auditMaxLimit      = 1000
	auditRotatedSuffix = ".1"
)

// auditEntry is one mutating request. It records the actor from the scoped token, never the token itself.
type auditEntry struct {
	Time      time.Time `json:"time"`
	Actor     string    `json:"actor,omitempty"`
	Role      string    `json:"role,omitempty"`
	Action    string    `json:"action"`
	App       string    `json:"app,omitempty"`
	Status    int       `json:"status"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Remote    string    `json:"remote,omitempty"`
	RequestID string    `json:"requestId,omitempty"`
}

type auditResponse struct {
	Entries []auditEntry `json:"entries"`
}

// auditStore appends JSON lines and rotates once at the size cap, so an active operator cannot fill the state volume with audit text.
type auditStore struct {
	path string

	mu   sync.Mutex
	size int64
}

func newAuditStore(path string) (*auditStore, error) {
	if path == "" {
		return nil, errors.New("audit path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}

	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}

	store := &auditStore{path: path}
	if info, err := os.Stat(path); err == nil {
		store.size = info.Size()

		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("audit file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("audit file: %w", err)
	}

	return store, nil
}

// append writes one JSON line, rotating the log once at the size cap.
func (a *auditStore) append(entry auditEntry) error {
	if a == nil {
		return nil
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	data = append(data, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.size+int64(len(data)) > auditMaxBytes {
		_ = os.Remove(a.path + auditRotatedSuffix)
		if err := os.Rename(a.path, a.path+auditRotatedSuffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate audit log: %w", err)
		}

		a.size = 0
	}

	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}

	written, writeErr := file.Write(data)

	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}

	a.size += int64(written)

	return writeErr
}

// tail returns the newest entries, up to the requested count.
func (a *auditStore) tail(limit int) ([]auditEntry, error) {
	if a == nil {
		return []auditEntry{}, nil
	}

	if limit <= 0 {
		limit = auditDefaultLimit
	}

	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}

	a.mu.Lock()
	data, err := os.ReadFile(a.path)
	a.mu.Unlock()

	if os.IsNotExist(err) {
		return []auditEntry{}, nil
	}

	if err != nil {
		return nil, err
	}

	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	start := max(len(lines)-limit, 0)

	out := make([]auditEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}

		var entry auditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		out = append(out, entry)
	}

	return out, nil
}

// auditWriter captures the status and a bounded error body for the audit entry without changing the response.
type auditWriter struct {
	http.ResponseWriter

	status int
	body   bytes.Buffer
}

func (w *auditWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *auditWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	if remaining := auditBodyBytes - w.body.Len(); remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}

		_, _ = w.body.Write(data[:remaining])
	}

	return w.ResponseWriter.Write(data)
}

func (w *auditWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *auditWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// auditAction names the mutating app request: deploy or delete on the app root, otherwise the route after the app name.
func auditAction(method, action string) string {
	if action != "" {
		return action
	}

	switch method {
	case http.MethodPost:
		return "deploy"
	case http.MethodDelete:
		return "delete"
	default:
		return method
	}
}

// remoteHost strips the port from a request's remote address.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}

	return r.RemoteAddr
}

// auditError extracts the error field from a response body.
func auditError(body []byte) string {
	var result struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err == nil {
		return result.Error
	}

	return ""
}

// appendAudit records one operator action after redacting app secrets.
func (s *supervisor) appendAudit(identity authIdentity, r *http.Request, action, app string, status int, cause string) {
	if s.audit == nil {
		return
	}

	if app != "" {
		cause = s.redactAppText(app, cause)
	}

	remote := remoteHost(r)
	if client := s.forwardedInfo(r).ClientIP; client != "" {
		remote = client
	}

	entry := auditEntry{
		Time:      time.Now().UTC(),
		Actor:     identity.Name,
		Role:      identity.Role,
		Action:    action,
		App:       app,
		Status:    status,
		OK:        status < http.StatusBadRequest,
		Error:     cause,
		Remote:    remote,
		RequestID: r.Header.Get(requestIDHeader),
	}
	if err := s.audit.append(entry); err != nil {
		log.Printf("audit: %v", err)
	}
}

// handleAudit serves the newest audit entries to an admin.
func (s *supervisor) handleAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	if s.audit == nil {
		writeError(w, http.StatusServiceUnavailable, "audit log unavailable")
		return
	}

	limit := auditDefaultLimit

	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > auditMaxLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(auditMaxLimit))
			return
		}

		limit = parsed
	}

	entries, err := s.audit.tail(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, auditResponse{Entries: entries})
}
