package main

import (
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:generate sh -c "cd web && bun install --frozen-lockfile && bun run build"

//go:embed all:web/build
var adminFiles embed.FS

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
	apps := s.allApps()
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
	if !requireMethod(w, r, http.MethodGet) {
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

// requireApp authenticates the request and returns the named app, writing 401 or 404 when it cannot.
func (s *supervisor) requireApp(w http.ResponseWriter, r *http.Request, name string) (*app, bool) {
	if !s.requireAuth(w, r) {
		return nil, false
	}
	a := s.lookup(name)
	if a == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return nil, false
	}
	return a, true
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
	s.startSchedule(a)
	a.sampleResources(time.Now())
	writeJSON(w, http.StatusOK, s.info(a))
}

// maxBackupRequestBytes caps the small JSON body used to change backup settings.
const maxBackupRequestBytes = 4 << 10

func (s *supervisor) writeBackups(w http.ResponseWriter, name, created string) {
	overview, err := s.backups.overview(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	overview.Created = created
	writeJSON(w, http.StatusOK, overview)
}

func (s *supervisor) handleBackupSettings(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodPut) {
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

func (s *supervisor) handleBackupRestore(w http.ResponseWriter, r *http.Request, name, file string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.backups.restore(r.Context(), name, file); err != nil {
		writeBackupError(w, err)
		return
	}
	s.writeBackups(w, name, "")
}

// handleBackupRoute serves the app's database snapshots, their policy and restores.
func (s *supervisor) handleBackupRoute(w http.ResponseWriter, r *http.Request, name, rest string) {
	if _, ok := s.requireApp(w, r, name); !ok {
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
