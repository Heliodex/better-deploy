// Command wt-supervisor runs WASI HTTP components with wasmtime and routes requests by host.
//
// Each app lives in its own directory under WT_APPS_DIR with an app.json, a main.wasm and optionally a precompiled main.cwasm. The supervisor starts one `wasmtime serve` process per app on a loopback port, proxies by Host, and deploys new bundles blue/green.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxUploadBytes  = 256 << 20
	maxExtractBytes = 512 << 20
	maxTarFiles     = 10_000
	readyTimeout    = 15 * time.Second
	stopTimeout     = 5 * time.Second
)

type config struct {
	appsDir, stateDir, listen, storageListen, domain string
	wasmtime, wasmtimeTarget, token, tlsCert, tlsKey string
}

type appConfig struct {
	Name     string            `json:"name"`
	Host     string            `json:"host"`
	Health   string            `json:"health"`
	Env      map[string]string `json:"env"`
	MemoryMB int               `json:"memory_mb,omitempty"`
}

type app struct {
	name, dir string
	port      int
	cfg       appConfig
	proxy     *httputil.ReverseProxy
	started   time.Time
	delay     time.Duration
	timer     *time.Timer

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	exited  bool
	retired bool
}

type supervisor struct {
	cfg     config
	storage *storageServer

	mu          sync.RWMutex
	apps, hosts map[string]*app

	deployMu sync.Mutex
	stopping atomic.Bool
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func configFromEnv() config {
	return config{
		appsDir:        env("WT_APPS_DIR", "./apps"),
		stateDir:       env("WT_STATE_DIR", "./state"),
		listen:         env("WT_LISTEN", "127.0.0.1:8080"),
		storageListen:  env("WT_STORAGE_LISTEN", "127.0.0.1:0"),
		domain:         env("WT_APPS_DOMAIN", "localhost"),
		wasmtime:       env("WT_WASMTIME", "wasmtime"),
		wasmtimeTarget: env("WT_WASMTIME_TARGET", "x86_64-unknown-linux-gnu"),
		token:          os.Getenv("WT_TOKEN"),
		tlsCert:        os.Getenv("WT_TLS_CERT"),
		tlsKey:         os.Getenv("WT_TLS_KEY"),
	}
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, " /:@") || strings.HasPrefix(host, ".") ||
		strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func defaultHost(name, domain string) string {
	if domain != "" {
		return name + "." + domain
	}
	return name + ".localhost"
}

func validateAppHost(host string) error {
	if !validHost(host) {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

func readAppConfig(dir, name, domain string) (appConfig, error) {
	cfg := appConfig{Name: name, Env: map[string]string{}}

	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("app.json: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return cfg, fmt.Errorf("read app.json: %w", err)
	}

	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = name
	}
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = defaultHost(name, domain)
	}
	cfg.Host = strings.ToLower(strings.TrimSpace(cfg.Host))
	if err := validateAppHost(cfg.Host); err != nil {
		return cfg, err
	}
	if cfg.Health == "" {
		cfg.Health = "/"
	}
	if !strings.HasPrefix(cfg.Health, "/") {
		return cfg, fmt.Errorf("health must start with /")
	}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	return cfg, nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func minimalEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}
	env := make([]string, 0, len(keep))
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *supervisor) appArgs(a *app, source string, allowPrecompiled bool) []string {
	args := []string{
		"serve",
		"-Scli",
		"-Sp3",
		"-S", "inherit-network",
		"-W", "component-model-async=y",
		"--target", s.cfg.wasmtimeTarget,
		"--addr", fmt.Sprintf("127.0.0.1:%d", a.port),
	}
	if allowPrecompiled {
		args = append(args, "--allow-precompiled")
	}
	stateDir := filepath.Join(s.cfg.stateDir, a.name)
	args = append(args, "--dir", stateDir+"::/data")
	for _, key := range sortedKeys(a.cfg.Env) {
		args = append(args, "--env", key+"="+a.cfg.Env[key])
	}
	if a.cfg.MemoryMB > 0 {
		args = append(args, "-W", fmt.Sprintf("max-memory-size=%d", a.cfg.MemoryMB<<20))
	}
	if s.storage != nil {
		args = append(args,
			"--env", "WT_STORAGE_URL="+s.storage.url(),
			"--env", "WT_STORAGE_TOKEN="+s.storage.tokenFor(a.name),
		)
	}
	return append(args, source)
}

func (s *supervisor) newProxy(a *app) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", a.port)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		host := r.Host
		proto := r.Header.Get("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
			if r.TLS != nil {
				proto = "https"
			}
		}
		director(r)
		r.Host = host
		r.Header.Set("X-Forwarded-Host", host)
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("app %s: proxy: %v", a.name, err)
		http.Error(w, "app unavailable", http.StatusBadGateway)
	}
	return proxy
}

func (s *supervisor) waitReady(a *app, done <-chan struct{}) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", a.port, a.cfg.Health)
	deadline := time.Now().Add(readyTimeout)

	for {
		select {
		case <-done:
			return fmt.Errorf("wasmtime exited before %s answered", target)
		default:
		}
		resp, err := client.Get(target)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no response on %s within %s", target, readyTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *supervisor) kill(a *app) {
	a.mu.Lock()
	cmd := a.cmd
	done := a.done
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}

	log.Printf("app %s: stopping pid %d", a.name, cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-done
	}
}

func (s *supervisor) stop(a *app) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.retired = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	s.kill(a)
}

func (s *supervisor) cancelRestart(a *app) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

func (s *supervisor) scheduleRestart(a *app, delay time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retired || s.stopping.Load() {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(delay, func() {
		if !s.stopping.Load() {
			s.restart(a)
		}
	})
}

func (s *supervisor) scheduleRetry(a *app) {
	a.mu.Lock()
	delay := a.delay
	if delay < time.Second {
		delay = time.Second
	}
	a.mu.Unlock()
	s.scheduleRestart(a, delay)
}

func (s *supervisor) restart(a *app) {
	a.mu.Lock()
	if a.retired || s.stopping.Load() || a.cmd != nil {
		a.mu.Unlock()
		return
	}
	dir := a.dir
	a.mu.Unlock()

	cwasm := filepath.Join(dir, "main.cwasm")
	wasm := filepath.Join(dir, "main.wasm")
	if _, err := os.Stat(cwasm); err == nil {
		err := s.launch(a, cwasm, true)
		if err == nil {
			return
		}
		log.Printf("app %s: precompiled component did not restart, falling back to wasm: %v", a.name, err)
		s.kill(a)
		s.cancelRestart(a)
	}
	if _, err := os.Stat(wasm); err != nil {
		log.Printf("app %s: restart: %v", a.name, err)
		s.scheduleRetry(a)
		return
	}
	if err := s.launch(a, wasm, false); err != nil {
		log.Printf("app %s: restart: %v", a.name, err)
		s.kill(a)
		s.cancelRestart(a)
		s.scheduleRetry(a)
	}
}

func (s *supervisor) launch(a *app, source string, allowPrecompiled bool) error {
	cmd := exec.Command(s.cfg.wasmtime, s.appArgs(a, source, allowPrecompiled)...)
	cmd.Env = minimalEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Stdout = log.New(os.Stderr, "["+a.name+"] ", 0).Writer()
	cmd.Stderr = log.New(os.Stderr, "["+a.name+"] ", 0).Writer()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start wasmtime: %w", err)
	}

	done := make(chan struct{})
	a.mu.Lock()
	a.cmd = cmd
	a.done = done
	a.exited = false
	a.started = time.Now().UTC()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()

	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		if a.cmd != cmd {
			a.mu.Unlock()
			close(done)
			return
		}
		a.cmd = nil
		a.exited = true
		ran := time.Since(a.started)
		if ran > 30*time.Second {
			a.delay = time.Second
		} else {
			a.delay *= 2
			if a.delay > 30*time.Second {
				a.delay = 30 * time.Second
			} else if a.delay < time.Second {
				a.delay = time.Second
			}
		}
		retired := a.retired
		delay := a.delay
		a.mu.Unlock()
		close(done)

		if err != nil {
			log.Printf("app %s: wasmtime exited: %v", a.name, err)
		}
		if retired || s.stopping.Load() {
			return
		}
		log.Printf("app %s: restarting in %s", a.name, delay)
		s.scheduleRestart(a, delay)
	}()

	if err := s.waitReady(a, done); err != nil {
		return err
	}
	a.proxy = s.newProxy(a)
	log.Printf("app %s started on 127.0.0.1:%d (%s)", a.name, a.port, a.cfg.Host)
	return nil
}

func (s *supervisor) spawn(name, dir string, cfg appConfig, port int) (*app, error) {
	if err := os.MkdirAll(filepath.Join(s.cfg.stateDir, name), 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	newApp := func() *app {
		return &app{
			name:    name,
			dir:     dir,
			port:    port,
			cfg:     cfg,
			done:    make(chan struct{}),
			started: time.Now().UTC(),
			delay:   time.Second,
		}
	}

	cwasm := filepath.Join(dir, "main.cwasm")
	wasm := filepath.Join(dir, "main.wasm")
	if _, statErr := os.Stat(cwasm); statErr == nil {
		a := newApp()
		err := s.launch(a, cwasm, true)
		if err == nil {
			return a, nil
		}
		log.Printf("app %s: precompiled component did not start, falling back to wasm: %v", name, err)
		s.stop(a)
	}
	if _, err := os.Stat(wasm); err != nil {
		return nil, fmt.Errorf("read main.wasm: %w", err)
	}
	a := newApp()
	if err := s.launch(a, wasm, false); err != nil {
		s.stop(a)
		return nil, err
	}
	return a, nil
}

func (s *supervisor) register(a *app) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old := s.apps[a.name]; old != nil {
		delete(s.hosts, old.cfg.Host)
	}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a
}

func (s *supervisor) hostTaken(host, except string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, a := range s.apps {
		if name != except && a.cfg.Host == host {
			return true
		}
	}
	return false
}

func (s *supervisor) loadApps() error {
	entries, err := os.ReadDir(s.cfg.appsDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(s.cfg.appsDir, 0o755)
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validName(name) {
			continue
		}
		dir := filepath.Join(s.cfg.appsDir, name)
		cfg, err := readAppConfig(dir, name, s.cfg.domain)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		if s.hostTaken(cfg.Host, name) {
			log.Printf("app %s: host %q is already used; skipped", name, cfg.Host)
			continue
		}
		port, err := freePort()
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		a, err := s.spawn(name, dir, cfg, port)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		s.register(a)
	}
	return nil
}

func (s *supervisor) authorized(r *http.Request) bool {
	if s.cfg.token == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type appInfo struct {
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Status    string    `json:"status"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
}

func (s *supervisor) info(a *app) appInfo {
	info := appInfo{Name: a.name, Host: a.cfg.Host, Port: a.port, StartedAt: a.started}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil && !a.exited {
		info.Status = "running"
		info.PID = a.cmd.Process.Pid
	} else {
		info.Status = "stopped"
	}
	return info
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
		}
	}
	return nil
}

func (s *supervisor) deploy(w http.ResponseWriter, r *http.Request, name string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

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

	archive, err := os.CreateTemp(s.cfg.appsDir, ".upload-*.tar.gz")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create temp file")
		return
	}
	archiveName := archive.Name()
	defer os.Remove(archiveName)
	if _, err := io.Copy(archive, io.LimitReader(file, maxUploadBytes)); err != nil {
		archive.Close()
		writeError(w, http.StatusBadRequest, "read artifact: "+err.Error())
		return
	}
	if err := archive.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "close temp file")
		return
	}

	tmpDir, err := os.MkdirTemp(s.cfg.appsDir, "."+name+".upload-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create upload directory")
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(archiveName, tmpDir); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "main.wasm")); err != nil {
		writeError(w, http.StatusBadRequest, "archive must contain main.wasm")
		return
	}

	cfg, err := readAppConfig(tmpDir, name, s.cfg.domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Name = name
	if s.hostTaken(cfg.Host, name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("host %q is already used by another app", cfg.Host))
		return
	}

	s.mu.RLock()
	old := s.apps[name]
	s.mu.RUnlock()

	port, err := freePort()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "allocate port")
		return
	}
	next, err := s.spawn(name, tmpDir, cfg, port)
	if err != nil {
		log.Printf("deploy %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "new version did not start: "+err.Error())
		return
	}

	finalDir := filepath.Join(s.cfg.appsDir, name)
	if err := replaceDir(tmpDir, finalDir); err != nil {
		s.stop(next)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.dir = finalDir

	s.register(next)
	if old != nil {
		s.stop(old)
	}

	log.Printf("deployed %s at %s (127.0.0.1:%d)", name, cfg.Host, port)
	writeJSON(w, http.StatusOK, s.info(next))
}

func (s *supervisor) handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
	writeJSON(w, http.StatusOK, out)
}

func (s *supervisor) handleApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/apps/")
	if name == "" || strings.Contains(name, "/") || !validName(name) {
		http.Error(w, "invalid app name", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		a := s.apps[name]
		s.mu.RUnlock()
		if a == nil {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, s.info(a))
	case http.MethodPost:
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wt-supervisor"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.deploy(w, r, name)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *supervisor) handleProxy(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s.mu.RLock()
	a := s.hosts[host]
	s.mu.RUnlock()
	if a == nil {
		http.Error(w, "no app for this host", http.StatusNotFound)
		return
	}
	a.proxy.ServeHTTP(w, r)
}

func (s *supervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/apps/", s.handleApp)
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

func (s *supervisor) stopAll() {
	s.mu.RLock()
	apps := make([]*app, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, a := range apps {
		wg.Add(1)
		go func(a *app) {
			defer wg.Done()
			s.stop(a)
		}(a)
	}
	wg.Wait()
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := configFromEnv()
	flag.StringVar(&cfg.listen, "listen", cfg.listen, "public listen address")
	flag.StringVar(&cfg.appsDir, "apps", cfg.appsDir, "directory containing one directory per app")
	flag.StringVar(&cfg.stateDir, "state", cfg.stateDir, "per-app state directory")
	flag.StringVar(&cfg.domain, "domain", cfg.domain, "default app host suffix")
	flag.StringVar(&cfg.wasmtime, "wasmtime", cfg.wasmtime, "wasmtime binary")
	flag.StringVar(&cfg.wasmtimeTarget, "wasmtime-target", cfg.wasmtimeTarget, "wasmtime target triple (must match the compiler)")
	flag.StringVar(&cfg.token, "token", cfg.token, "deploy bearer token (empty disables auth)")
	flag.StringVar(&cfg.tlsCert, "tls-cert", cfg.tlsCert, "TLS certificate (with --tls-key)")
	flag.StringVar(&cfg.tlsKey, "tls-key", cfg.tlsKey, "TLS private key (with --tls-cert)")
	flag.Parse()

	absoluteApps, err := filepath.Abs(cfg.appsDir)
	if err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	absoluteState, err := filepath.Abs(cfg.stateDir)
	if err != nil {
		log.Fatalf("state dir: %v", err)
	}
	cfg.appsDir, cfg.stateDir = absoluteApps, absoluteState
	if err := os.MkdirAll(cfg.appsDir, 0o755); err != nil {
		log.Fatalf("apps dir: %v", err)
	}
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}

	storage, err := newStorageServer(filepath.Join(cfg.stateDir, ".storage"), cfg.storageListen)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	s := &supervisor{
		cfg:     cfg,
		storage: storage,
		apps:    map[string]*app{},
		hosts:   map[string]*app{},
	}
	if err := s.loadApps(); err != nil {
		log.Fatalf("load apps: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		s.stopping.Store(true)
		log.Printf("shutting down")
		_ = server.Close()
	}()

	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatalf("--tls-cert and --tls-key must be set together")
	}
	log.Printf("supervisor listening on %s with %d app(s)", cfg.listen, len(s.apps))
	if cfg.tlsCert != "" {
		err = server.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	s.stopAll()
	storage.close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
