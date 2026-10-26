package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxUploadBytes  = 256 << 20
	maxExtractBytes = 512 << 20
	maxTarFiles     = 10_000

	deployDrain = 2 * time.Second
	// deployRespondAfter keeps the request inside proxy timeouts. A cold Wasm compile takes longer than that, so the
	// deploy carries on in the background and clients poll GET /apps/<name> until the app reports the new process.
	deployRespondAfter = 5 * time.Second
)

var errDeployNotReady = errors.New("new version did not become ready")

type appInfo struct {
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Status    string    `json:"status"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func appInfoFor(app *App) appInfo {
	info := appInfo{
		Name: app.Name,
		Host: app.Host,
		Port: app.Port,
	}

	app.mu.Lock()
	defer app.mu.Unlock()
	if app.cmd == nil || app.cmd.Process == nil {
		info.Status = "stopped"
		return info
	}
	info.Status = "running"
	info.PID = app.cmd.Process.Pid
	info.StartedAt = app.startedAt
	return info
}

func (s *Supervisor) getApp(name string) *App {
	s.appsMu.RLock()
	defer s.appsMu.RUnlock()
	return s.apps[name]
}

func (s *Supervisor) hostTaken(host, except string) string {
	s.appsMu.RLock()
	defer s.appsMu.RUnlock()
	for name, app := range s.apps {
		if name != except && app.Host == host {
			return name
		}
	}
	return ""
}

func (s *Supervisor) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	want := s.cfg.token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func extractTarGz(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	var files int

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry %q escapes the app directory", header.Name)
		}

		target := filepath.Join(dst, filepath.FromSlash(name))
		rel, err := filepath.Rel(dst, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the app directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			files++
			if files > maxTarFiles {
				return fmt.Errorf("archive has too many entries")
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create directory %s: %w", name, err)
			}
		case tar.TypeReg:
			files++
			if files > maxTarFiles {
				return fmt.Errorf("archive has too many entries")
			}
			if header.Size < 0 || total+header.Size > maxExtractBytes {
				return fmt.Errorf("archive is too large")
			}
			total += header.Size

			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent of %s: %w", name, err)
			}
			mode := os.FileMode(0o644)
			if header.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			_, copyErr := io.CopyN(out, tr, header.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return fmt.Errorf("write %s: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %s: %w", name, closeErr)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive entry %q is a link, which is not allowed", header.Name)
		default:
			// Ignore device nodes, FIFOs and other special entries.
		}
	}
	return nil
}

func preserveAppJSON(tmp, oldDir string) error {
	dst := filepath.Join(tmp, "app.json")
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat app.json: %w", err)
	}

	if oldDir != "" {
		data, err := os.ReadFile(filepath.Join(oldDir, "app.json"))
		switch {
		case err == nil:
			return os.WriteFile(dst, data, 0o644)
		case !os.IsNotExist(err):
			return fmt.Errorf("read old app.json: %w", err)
		}
	}
	return os.WriteFile(dst, []byte("{}\n"), 0o644)
}

func oldDir(app *App) string {
	if app == nil {
		return ""
	}
	return app.Dir
}

func replaceDir(src, dst string) error {
	backup := ""
	if _, err := os.Stat(dst); err == nil {
		backup = filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".old-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("move old app aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat app directory: %w", err)
	}

	if err := os.Rename(src, dst); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dst)
		}
		return fmt.Errorf("install app: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			log.Printf("remove old app %s: %v", backup, err)
		}
	}
	return nil
}

func (s *Supervisor) deployApp(name, archive string) (*App, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid app name %q", name)
	}

	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	old := s.getApp(name)

	tmp, err := os.MkdirTemp(s.cfg.appsDir, "."+name+".upload-")
	if err != nil {
		return nil, fmt.Errorf("create upload directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := extractTarGz(archive, tmp); err != nil {
		return nil, err
	}

	manifest := filepath.Join(tmp, "spin.toml")
	if info, err := os.Stat(manifest); err != nil || info.IsDir() {
		return nil, fmt.Errorf("archive must contain spin.toml at its root")
	}

	if err := preserveAppJSON(tmp, oldDir(old)); err != nil {
		return nil, err
	}

	ac, err := readAppConfig(tmp, name, s.cfg.appsDomain)
	if err != nil {
		return nil, err
	}
	if s.cfg.deployHost != "" && ac.Host == s.cfg.deployHost {
		return nil, fmt.Errorf("host %q is reserved for the deploy API", ac.Host)
	}
	if other := s.hostTaken(ac.Host, name); other != "" {
		return nil, fmt.Errorf("host %q is already used by %s", ac.Host, other)
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port: %w", err)
	}

	app := &App{
		Name:     name,
		Host:     ac.Host,
		Port:     port,
		Dir:      tmp,
		StateDir: filepath.Join(s.cfg.stateDir, name),
		Health:   ac.Health,
		Env:      ac.Env,
		Args:     ac.Args,
		delay:    time.Second,
	}

	// Blue/green: the new version starts on its own port and compiles its Wasm while the old one keeps serving.
	// Caddy only switches to the new port once it answers its health path, so requests never hit a dead upstream.
	if old != nil {
		s.retireApp(old)
	}
	s.startAppWait(app, false)
	if err := s.awaitApp(app, s.cfg.deployReadyTimeout); err != nil {
		s.retireApp(app)
		s.stopApp(app)
		if old != nil {
			s.unretireApp(old)
			s.startApp(old)
		}
		return nil, fmt.Errorf("%w: %v", errDeployNotReady, err)
	}

	finalDir := filepath.Join(s.cfg.appsDir, name)
	if err := replaceDir(tmp, finalDir); err != nil {
		s.retireApp(app)
		s.stopApp(app)
		if old != nil {
			s.unretireApp(old)
			s.startApp(old)
		}
		return nil, err
	}
	app.mu.Lock()
	app.Dir = finalDir
	app.mu.Unlock()

	s.appsMu.Lock()
	s.apps[name] = app
	s.appsMu.Unlock()

	if err := s.applyCaddyConfig(); err != nil {
		log.Printf("deploy %s: apply Caddy config: %v", name, err)
	}
	if old != nil {
		// Give Caddy a moment to send new requests to the new port before the old process goes away.
		time.Sleep(deployDrain)
		s.stopApp(old)
	}

	return app, nil
}

func (s *Supervisor) beginDeploy(name string) bool {
	s.deployingMu.Lock()
	defer s.deployingMu.Unlock()
	if s.deploying[name] {
		return false
	}
	s.deploying[name] = true
	return true
}

func (s *Supervisor) endDeploy(name string) {
	s.deployingMu.Lock()
	delete(s.deploying, name)
	s.deployingMu.Unlock()
}

func (s *Supervisor) handleDeploy(w http.ResponseWriter, r *http.Request, name string) {
	if s.cfg.token == "" {
		writeError(w, http.StatusServiceUnavailable, "deploy disabled: SUPERVISOR_TOKEN is not set")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="spin-supervisor"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing artifact field")
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp(s.cfg.tempDir, "upload-*.tar.gz")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create temp file")
		return
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, io.LimitReader(file, maxUploadBytes)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		writeError(w, http.StatusBadRequest, "read artifact: "+err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		writeError(w, http.StatusInternalServerError, "close temp file")
		return
	}

	if !s.beginDeploy(name) {
		os.Remove(tmpName)
		writeError(w, http.StatusConflict, "a deploy for this app is already in progress")
		return
	}

	id := r.URL.Query().Get("deploy")
	if id == "" {
		id = randomDeployID()
	}
	if !validDeployID(id) {
		s.endDeploy(name)
		os.Remove(tmpName)
		writeError(w, http.StatusBadRequest, "invalid deploy id")
		return
	}
	record := s.newDeploy(id, name)
	w.Header().Set("Location", "/deploys/"+id)

	type deployResult struct {
		app *App
		err error
	}
	result := make(chan deployResult, 1)
	go func() {
		defer s.endDeploy(name)
		defer os.Remove(tmpName)
		app, err := s.deployApp(name, tmpName)
		if err != nil {
			log.Printf("deploy %s (%s): %v", id, name, err)
		}
		if err == nil {
			info := appInfoFor(app)
			s.finishDeploy(record, &info, nil)
		} else {
			s.finishDeploy(record, nil, err)
		}
		result <- deployResult{app: app, err: err}
	}()

	select {
	case res := <-result:
		if res.err != nil {
			status := http.StatusBadRequest
			if errors.Is(res.err, errDeployNotReady) {
				status = http.StatusGatewayTimeout
			}
			writeError(w, status, res.err.Error())
			return
		}
		log.Printf("deploy %s: now %s at https://%s (127.0.0.1:%d)", name, res.app.Dir, res.app.Host, res.app.Port)
		writeJSON(w, http.StatusOK, appInfoFor(res.app))
	case <-time.After(deployRespondAfter):
		log.Printf("deploy %s (%s): still starting; the old version keeps serving", id, name)
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "name": name, "status": deployStarting})
	}
}

func (s *Supervisor) handleApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/apps/")
	if name == "" || strings.Contains(name, "/") || !validName(name) {
		writeError(w, http.StatusBadRequest, "invalid app name")
		return
	}

	switch r.Method {
	case http.MethodGet:
		app := s.getApp(name)
		if app == nil {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
		writeJSON(w, http.StatusOK, appInfoFor(app))
	case http.MethodPost:
		s.handleDeploy(w, r, name)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Supervisor) handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	s.appsMu.RLock()
	apps := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.appsMu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })

	out := make([]appInfo, 0, len(apps))
	for _, app := range apps {
		out = append(out, appInfoFor(app))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Supervisor) startAPI() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/apps/", s.handleApp)
	mux.HandleFunc("/deploys/", s.handleDeployStatus)

	listener, err := net.Listen("tcp", s.cfg.apiListen)
	if err != nil {
		return err
	}

	s.api = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		err := s.api.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !s.stopping.Load() {
			log.Printf("api: %v", err)
			close(s.apiExited)
		}
	}()

	log.Printf("api listening on %s", s.cfg.apiListen)
	return nil
}
