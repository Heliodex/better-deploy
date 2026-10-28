package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
)

// appLog appends an app's output to a capped log file for the admin panel. The
// file lives outside the app's /data preopen so components cannot read it.
type appLog struct {
	mu   sync.Mutex
	dir  string
	file *os.File
	size int64
}

func newAppLog(dir string) (*appLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := &appLog{dir: dir}
	if err := out.open(); err != nil {
		return nil, err
	}
	return out, nil
}

func (l *appLog) path() string {
	return filepath.Join(l.dir, "output.log")
}

func (l *appLog) backupPath() string {
	return filepath.Join(l.dir, "output.log.1")
}

func (l *appLog) open() error {
	file, err := os.OpenFile(l.path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	l.file = file
	l.size = info.Size()
	return nil
}

// Write implements io.Writer, rotating to output.log.1 past maxLogBytes.
func (l *appLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogBytes {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := l.file.Write(p)
	l.size += int64(written)
	return written, err
}

func (l *appLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	_ = os.Remove(l.backupPath())
	if err := os.Rename(l.path(), l.backupPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.open()
}

// tail returns the last n log lines, oldest first.
func (l *appLog) tail(n int) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []string{}
	for _, path := range []string{l.backupPath(), l.path()} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, strings.Split(string(data), "\n")...)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

type logsResponse struct {
	Logs []string `json:"logs"`
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

func (s *supervisor) handleLogs(w http.ResponseWriter, r *http.Request, a *app) {
	tail := defaultLogTail
	if value := r.URL.Query().Get("tail"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maxLogTail {
			writeError(w, http.StatusBadRequest, "tail must be between 1 and 5000")
			return
		}
		tail = parsed
	}
	logs := []string{}
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

func (s *supervisor) handleRestart(w http.ResponseWriter, r *http.Request, a *app) {
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
	writeJSON(w, http.StatusOK, s.info(a))
}

// adminStatic serves the embedded admin panel. SvelteKit emits root-relative
// URLs under the /admin base path, so the mux strips the prefix first.
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
		}
		files.ServeHTTP(w, r)
	})
}
