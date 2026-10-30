package main

import (
	"bytes"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:generate sh -c "cd web && bun install --frozen-lockfile && bun run build"

//go:embed all:web/build
var adminFiles embed.FS

const (
	defaultLogTail = 200
	maxLogTail     = 5000
	maxLogBytes    = 1 << 20
	logTimeFormat  = time.RFC3339Nano
)

// appLog appends an app's output to capped log files for the admin panel. Files live outside the app's /data preopen so components cannot read them. Each line is stamped with the time the supervisor received it, so timestamps do not depend on how an app logs. The app's own output goes to output.log; supervisor-side lines for the app, such as proxied requests, go to supervisor.log so the two read apart.
type appLog struct {
	mu             sync.Mutex
	dir            string
	file           *os.File
	size           int64
	supervisorFile *os.File
	supervisorSize int64
	pending        []byte
	subs           map[chan logEntry]struct{}
}

func newAppLog(dir string) (*appLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := &appLog{dir: dir, subs: map[chan logEntry]struct{}{}}
	if err := out.open(); err != nil {
		return nil, err
	}
	return out, nil
}

func (l *appLog) subscribe() chan logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	messages := make(chan logEntry, 128)
	l.subs[messages] = struct{}{}
	return messages
}

func (l *appLog) unsubscribe(messages chan logEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subs, messages)
}

// publish fans a line out to streaming clients. Callers hold l.mu, and delivery is non-blocking so a stalled client can never hold up the app's output.
func (l *appLog) publish(entry logEntry) {
	for messages := range l.subs {
		select {
		case messages <- entry:
		default:
		}
	}
}

func (l *appLog) path() string {
	return filepath.Join(l.dir, "output.log")
}

func (l *appLog) backupPath() string {
	return filepath.Join(l.dir, "output.log.1")
}

func (l *appLog) supervisorPath() string {
	return filepath.Join(l.dir, "supervisor.log")
}

func (l *appLog) supervisorBackupPath() string {
	return filepath.Join(l.dir, "supervisor.log.1")
}

func openLogFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func rotateLogFile(file *os.File, path, backup string) (*os.File, int64, error) {
	if err := file.Close(); err != nil {
		return nil, 0, err
	}
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err != nil && !os.IsNotExist(err) {
		return nil, 0, err
	}
	return openLogFile(path)
}

func (l *appLog) open() error {
	file, size, err := openLogFile(l.path())
	if err != nil {
		return err
	}
	l.file, l.size = file, size
	supervisorFile, supervisorSize, err := openLogFile(l.supervisorPath())
	if err != nil {
		_ = file.Close()
		return err
	}
	l.supervisorFile, l.supervisorSize = supervisorFile, supervisorSize
	return nil
}

// Write implements io.Writer. Output arrives in arbitrary chunks, so lines are buffered until their newline and each one is written with a receive timestamp.
func (l *appLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, p...)
	for {
		index := bytes.IndexByte(l.pending, '\n')
		if index < 0 {
			break
		}
		line := string(l.pending[:index])
		l.pending = l.pending[index+1:]
		if err := l.writeLine(line); err != nil {
			return 0, err
		}
	}
	// A writer that never emits a newline must not grow the buffer without bound.
	if len(l.pending) > maxLogBytes {
		l.pending = append([]byte{}, l.pending[len(l.pending)-maxLogBytes:]...)
	}
	return len(p), nil
}

// guestStderr matches wasmtime's prefix for a guest's own stderr/stdout, e.g. "stderr [1] :: ".
var guestStderr = regexp.MustCompile(`^(?:stderr|stdout) \[\d+\] :: `)

// writeLine stamps one complete line of the app's own output and appends it, rotating past maxLogBytes.
func (l *appLog) writeLine(line string) error {
	line = guestStderr.ReplaceAllString(line, "")
	level, message := splitLevelPrefix(line)
	stamp := time.Now().UTC().Format(logTimeFormat)
	data := []byte(stamp + " " + level + " " + message + "\n")
	if l.size+int64(len(data)) > maxLogBytes {
		file, size, err := rotateLogFile(l.file, l.path(), l.backupPath())
		if err != nil {
			return err
		}
		l.file, l.size = file, size
	}
	written, err := l.file.Write(data)
	l.size += int64(written)
	if err != nil {
		return err
	}
	l.publish(logEntry{Time: stamp, Level: strings.ToLower(level), Source: "app", Message: message})
	return nil
}

// logSupervisor records a supervisor-side line for the app, such as a proxied request.
// It streams and persists alongside the app's own output, marked so the two read apart.
func (l *appLog) logSupervisor(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stamp := time.Now().UTC().Format(logTimeFormat)
	data := []byte(stamp + " INFO " + message + "\n")
	if l.supervisorSize+int64(len(data)) > maxLogBytes {
		file, size, err := rotateLogFile(l.supervisorFile, l.supervisorPath(), l.supervisorBackupPath())
		if err != nil {
			return
		}
		l.supervisorFile, l.supervisorSize = file, size
	}
	written, err := l.supervisorFile.Write(data)
	l.supervisorSize += int64(written)
	if err != nil {
		return
	}
	l.publish(logEntry{Time: stamp, Level: "info", Source: "supervisor", Message: message})
}

// logLevels are the prefixes an app may put at the start of a line, most verbose first.
var logLevels = []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"}

// logEntry is one log line with the time the supervisor received it, its level, and
// which side wrote it: the app process or the supervisor. Lines written before
// sources existed have no marker and read as app output.
type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

// splitLevelPrefix pulls an app's "<LEVEL> " prefix off a line. Anything unrecognized is part of the message and the level is info.
func splitLevelPrefix(line string) (string, string) {
	token, rest, ok := strings.Cut(line, " ")
	if !ok {
		return "INFO", line
	}
	upper := strings.ToUpper(token)
	for _, level := range logLevels {
		if upper == level {
			return level, rest
		}
	}
	return "INFO", line
}

// parseLogLine splits a stamped line into its time and message.
func parseLogLine(line string) logEntry {
	return parseSourcedLogLine(line, "app")
}

// parseSourcedLogLine parses a line from one of the app's log files, marking which side wrote it.
func parseSourcedLogLine(line, source string) logEntry {
	stamp, rest, ok := strings.Cut(line, " ")
	if !ok {
		return logEntry{Level: "info", Source: source, Message: line}
	}
	if _, err := time.Parse(logTimeFormat, stamp); err != nil {
		return logEntry{Level: "info", Source: source, Message: line}
	}
	level, message := splitLevelPrefix(rest)
	return logEntry{Time: stamp, Level: strings.ToLower(level), Source: source, Message: message}
}

// tail returns the last n log lines from both the app and the supervisor, oldest first.
func (l *appLog) tail(n int) ([]logEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	files := []struct {
		backup, path, source string
	}{
		{l.backupPath(), l.path(), "app"},
		{l.supervisorBackupPath(), l.supervisorPath(), "supervisor"},
	}
	out := []logEntry{}
	for _, file := range files {
		for _, path := range []string{file.backup, file.path} {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for line := range strings.SplitSeq(string(data), "\n") {
				if line == "" {
					continue
				}
				out = append(out, parseSourcedLogLine(line, file.source))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

type logsResponse struct {
	Logs []logEntry `json:"logs"`
}

// hub fans out app snapshots to SSE subscribers. Snapshots carry full state, so a slow subscriber that misses an update converges on the next one.
type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{subs: map[chan []byte]struct{}{}}
}

func (h *hub) subscribe() chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	messages := make(chan []byte, 4)
	h.subs[messages] = struct{}{}
	return messages
}

func (h *hub) unsubscribe(messages chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, messages)
}

func (h *hub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

func (h *hub) publish(message []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for messages := range h.subs {
		select {
		case messages <- message:
		default:
		}
	}
}

type snapshotMessage struct {
	Type string    `json:"type"`
	Apps []appInfo `json:"apps"`
}

// snapshot returns the current app list, newest details included.
func (s *supervisor) snapshot() []appInfo {
	// Sampling here keeps REST and SSE snapshots current; the method throttles itself.
	s.sampleAll()
	s.mu.RLock()
	apps := make([]*app, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	s.mu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].name < apps[j].name })

	out := make([]appInfo, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.info(a))
	}
	return out
}

// broadcast sends the current snapshot to every SSE subscriber. Dropped messages are harmless: the next broadcast carries the newest state.
func (s *supervisor) broadcast() {
	if s.events == nil {
		return
	}
	data, err := json.Marshal(snapshotMessage{Type: "snapshot", Apps: s.snapshot()})
	if err != nil {
		return
	}
	s.events.publish(data)
}

// handleEvents streams app snapshots over SSE, with comment pings to keep the connection alive through proxies. EventSource cannot set headers, so browsers authenticate with the wt_token cookie instead of a bearer token.
func (s *supervisor) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wt-supervisor"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	data, err := json.Marshal(snapshotMessage{Type: "snapshot", Apps: s.snapshot()})
	if err != nil {
		http.Error(w, "snapshot failed", http.StatusInternalServerError)
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return
	}
	flusher.Flush()

	messages := s.events.subscribe()
	defer s.events.unsubscribe(messages)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case message, ok := <-messages:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", message); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// requireAuth writes 401 unless the request carries the deploy token.
func (s *supervisor) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="wt-supervisor"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// logTailParam reads the ?tail= query, writing 400 when it is out of range.
func logTailParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	value := r.URL.Query().Get("tail")
	if value == "" {
		return defaultLogTail, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maxLogTail {
		writeError(w, http.StatusBadRequest, "tail must be between 1 and 5000")
		return 0, false
	}
	return parsed, true
}

func (s *supervisor) handleLogs(w http.ResponseWriter, r *http.Request, a *app) {
	tail, ok := logTailParam(w, r)
	if !ok {
		return
	}
	logs := []logEntry{}
	if a.logs != nil {
		var err error
		logs, err = a.logs.tail(tail)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, logsResponse{Logs: logs})
}

// logStreamMessage is one SSE frame: the backlog on connect, then single lines.
type logStreamMessage struct {
	Type string     `json:"type"`
	Logs []logEntry `json:"logs,omitempty"`
	Log  *logEntry  `json:"log,omitempty"`
}

// handleLogStream streams an app's log: the recent backlog on connect, then each new line as the supervisor writes it, with comment pings to hold the connection.
func (s *supervisor) handleLogStream(w http.ResponseWriter, r *http.Request, a *app) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	tail, ok := logTailParam(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(message logStreamMessage) bool {
		data, err := json.Marshal(message)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	backlog := []logEntry{}
	var messages chan logEntry
	if a.logs != nil {
		var err error
		backlog, err = a.logs.tail(tail)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		messages = a.logs.subscribe()
		defer a.logs.unsubscribe(messages)
	}
	if !send(logStreamMessage{Type: "backlog", Logs: backlog}) {
		return
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case entry := <-messages:
			if !send(logStreamMessage{Type: "line", Log: &entry}) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *supervisor) handleRestart(w http.ResponseWriter, _ *http.Request, a *app) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a.mu.Lock()
	name, dir := a.name, a.dir
	a.mu.Unlock()
	s.stop(a)
	a.mu.Lock()
	a.retired = false
	a.delay = time.Second
	a.mu.Unlock()
	cwasm := filepath.Join(dir, "main.cwasm")
	if _, err := os.Stat(cwasm); err != nil {
		writeError(w, http.StatusInternalServerError, "app has no precompiled component")
		return
	}
	if err := s.launch(a, cwasm); err != nil {
		log.Printf("restart %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "app did not restart: "+err.Error())
		return
	}
	a.sampleResources(time.Now())
	writeJSON(w, http.StatusOK, s.info(a))
}

// maxBackupRequestBytes caps the small JSON body used to change backup settings.
const maxBackupRequestBytes = 4 << 10

// handleBackupRoute serves the app's database snapshots, their policy and restores.
func (s *supervisor) handleBackupRoute(w http.ResponseWriter, r *http.Request, name, rest string) {
	if !s.requireAuth(w, r) {
		return
	}
	if s.lookup(name) == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	if s.backups == nil {
		http.Error(w, "backups unavailable", http.StatusServiceUnavailable)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "":
		s.handleBackups(w, r, name)
	case rest == "config":
		s.handleBackupSettings(w, r, name)
	case strings.HasSuffix(rest, "/restore"):
		s.handleBackupRestore(w, r, name, strings.TrimSuffix(rest, "/restore"))
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

func (s *supervisor) handleBackups(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		s.writeBackups(w, name, "")
	case http.MethodPost:
		info, err := s.backups.create(r.Context(), name)
		if err != nil && !errors.Is(err, errBackupUnchanged) {
			writeBackupError(w, err)
			return
		}
		s.writeBackups(w, name, info.Name)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *supervisor) handleBackupSettings(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupRequestBytes)
	var settings backupSettings
	if err := json.UnmarshalRead(r.Body, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := settings.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.backups.saveSettings(name, settings); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeBackups(w, name, "")
}

func (s *supervisor) handleBackupRestore(w http.ResponseWriter, r *http.Request, name, file string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.backups.restore(r.Context(), name, file); err != nil {
		writeBackupError(w, err)
		return
	}
	s.writeBackups(w, name, "")
}

func (s *supervisor) writeBackups(w http.ResponseWriter, name, created string) {
	overview, err := s.backups.overview(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	overview.Created = created
	writeJSON(w, http.StatusOK, overview)
}

func writeBackupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoStorageDatabase), errors.Is(err, errInvalidBackupName):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errBackupNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// adminStatic serves the embedded admin panel. SvelteKit emits root-relative URLs under the /admin base path, so the mux strips the prefix first.
func adminStatic() http.Handler {
	sub, err := fs.Sub(adminFiles, "web/build")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "admin panel not built", http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_app/immutable/") {
			w.Header().Set("cache-control", "public, max-age=31536000, immutable")
		} else {
			// Entry HTML must revalidate so panels pick up new bundles after a supervisor update.
			w.Header().Set("cache-control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
