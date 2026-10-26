// Command supervisor runs several Spin applications in one container.
//
// It starts one `spin up` process per application, applies a Caddy config that routes each hostname to the app's loopback port, and keeps each app's key-value store and SQLite database in a directory on the volume.
// New versions arrive as gzipped tarballs over the deploy API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	volumeRoot string

	appsDir, stateDir, logDir, cacheDir, tempDir string

	caddyDataDir, caddyConfigDir string

	spinBin string

	listen, apiListen string

	appsDomain, deployHost, token string

	stopTimeout, deployReadyTimeout time.Duration
}

type appConfigFile struct {
	Host   string            `json:"host"`
	Health string            `json:"health"`
	Env    map[string]string `json:"env"`
	Args   []string          `json:"args"`
}

type App struct {
	Name, Host            string
	Dir, StateDir, Health string
	Port                  int
	Env                   map[string]string
	Args                  []string

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	startedAt time.Time
	delay     time.Duration
	retired   bool
}

type Supervisor struct {
	cfg config

	appsMu sync.RWMutex
	apps   map[string]*App

	caddyMu, deployMu sync.Mutex
	apiExited         chan struct{}

	api      *http.Server
	stopping atomic.Bool
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func configFromEnv() config {
	return config{
		volumeRoot:     env("SPIN_VOLUME_ROOT", "/var/lib/spin"),
		appsDir:        env("SPIN_APPS_DIR", "/var/lib/spin/apps"),
		stateDir:       env("SPIN_STATE_DIR", "/var/lib/spin/state"),
		logDir:         env("SPIN_LOG_DIR", "/var/lib/spin/logs"),
		cacheDir:       env("SPIN_CACHE_DIR", "/var/lib/spin/cache"),
		tempDir:        env("SPIN_TMP_DIR", "/var/lib/spin/tmp"),
		caddyDataDir:   env("CADDY_DATA_DIR", "/var/lib/spin/caddy/data"),
		caddyConfigDir: env("CADDY_CONFIG_DIR", "/var/lib/spin/caddy/config"),
		spinBin:        env("SPIN_BIN", "spin"),
		listen:         env("LISTEN", "0.0.0.0:8080"),
		apiListen:      env("API_LISTEN", "127.0.0.1:9090"),
		appsDomain:     strings.ToLower(strings.TrimSpace(os.Getenv("APPS_DOMAIN"))),
		deployHost:     strings.ToLower(strings.TrimSpace(os.Getenv("DEPLOY_HOST"))),
		token:          os.Getenv("SUPERVISOR_TOKEN"),
		stopTimeout:    time.Duration(envInt("SPIN_STOP_TIMEOUT_SECONDS", 10)) * time.Second,

		deployReadyTimeout: time.Duration(envInt("DEPLOY_READY_TIMEOUT_SECONDS", 180)) * time.Second,
	}
}

// minimalEnv keeps the process environment small so container-level secrets such as SUPERVISOR_TOKEN are not inherited by Spin.
// Application variables are passed explicitly with -e.
func minimalEnv(extra ...string) []string {
	keep := []string{
		"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	}
	env := make([]string, 0, len(keep)+len(extra))
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, extra...)
}

func setEnvDefault(key, value string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, value)
	}
}

func runtimeDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("runtime path must be absolute: %q", dir)
	}
	clean := filepath.Clean(dir)
	if clean == "/" {
		return "", fmt.Errorf("runtime path must not be /")
	}
	return clean, nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// prepareRuntime creates the writable directories under the volume root, hands them to the runtime user, and drops root privileges before any child starts.
// When the container is already running as an unprivileged user it only checks that the directories are writable.
func prepareRuntime(cfg config) error {
	const runUID, runGID = 1000, 1000

	setEnvDefault("HOME", "/var/lib/spin/home")
	setEnvDefault("TMPDIR", cfg.tempDir)
	_ = os.Setenv("XDG_DATA_HOME", cfg.caddyDataDir)
	_ = os.Setenv("XDG_CONFIG_HOME", cfg.caddyConfigDir)

	root, err := runtimeDir(cfg.volumeRoot)
	if err != nil {
		return err
	}

	dirs := []string{
		root,
		os.Getenv("HOME"),
		os.Getenv("TMPDIR"),
		cfg.appsDir,
		cfg.stateDir,
		cfg.logDir,
		cfg.cacheDir,
		cfg.tempDir,
		cfg.caddyDataDir,
		cfg.caddyConfigDir,
	}
	for i, dir := range dirs {
		clean, err := runtimeDir(dir)
		if err != nil {
			return err
		}
		dirs[i] = clean
		if err := os.MkdirAll(clean, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", clean, err)
		}
	}

	if os.Geteuid() == 0 {
		if err := chownTree(root, runUID, runGID); err != nil {
			return fmt.Errorf("chown %s: %w", root, err)
		}
		if err := syscall.Setgroups([]int{runGID}); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(runGID); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(runUID); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
		if os.Geteuid() != runUID {
			return fmt.Errorf("failed to drop privileges: uid is %d", os.Geteuid())
		}
	}

	for _, dir := range dirs {
		if err := syscall.Access(dir, 0x2); err != nil {
			return fmt.Errorf("%s is not writable by uid %d", dir, os.Geteuid())
		}
	}
	return nil
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

func validateHost(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("invalid host %q", host)
	}
	if strings.ContainsAny(host, " /:@") || strings.HasPrefix(host, ".") ||
		strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return fmt.Errorf("invalid host %q", host)
	}
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

func defaultHost(name, domain string) string {
	if domain != "" {
		return name + "." + domain
	}
	return name + ".localhost"
}

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func splitListen(listen string) (host, port string, err error) {
	listen = strings.TrimSpace(listen)
	listen = strings.TrimPrefix(listen, "http://")
	listen = strings.TrimPrefix(listen, "https://")

	if h, p, splitErr := net.SplitHostPort(listen); splitErr == nil {
		return h, p, nil
	}
	if strings.HasPrefix(listen, ":") && len(listen) > 1 {
		return "", listen[1:], nil
	}
	if listen != "" && !strings.Contains(listen, ":") {
		return "", listen, nil
	}
	return "", "", fmt.Errorf("invalid listen address %q", listen)
}

func (s *Supervisor) appCount() int {
	s.appsMu.RLock()
	defer s.appsMu.RUnlock()
	return len(s.apps)
}

func readAppConfig(dir, name, domain string) (appConfigFile, error) {
	var ac appConfigFile
	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &ac); err != nil {
			return ac, fmt.Errorf("app.json: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return ac, fmt.Errorf("read app.json: %w", err)
	}

	if strings.TrimSpace(ac.Host) == "" {
		ac.Host = defaultHost(name, domain)
	}
	ac.Host = strings.ToLower(strings.TrimSpace(ac.Host))
	if err := validateHost(ac.Host); err != nil {
		return ac, err
	}
	if ac.Health == "" {
		ac.Health = "/"
	}
	if !strings.HasPrefix(ac.Health, "/") {
		return ac, fmt.Errorf("health must start with /")
	}
	if strings.ContainsAny(ac.Health, " \t\r\n") {
		return ac, fmt.Errorf("health must not contain whitespace")
	}
	if ac.Env == nil {
		ac.Env = map[string]string{}
	}
	return ac, nil
}

func (s *Supervisor) newApp(name string, port int) (*App, error) {
	dir := filepath.Join(s.cfg.appsDir, name)
	ac, err := readAppConfig(dir, name, s.cfg.appsDomain)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, fmt.Errorf("allocate port: %w", err)
		}
	}
	return &App{
		Name:     name,
		Host:     ac.Host,
		Port:     port,
		Dir:      dir,
		StateDir: filepath.Join(s.cfg.stateDir, name),
		Health:   ac.Health,
		Env:      ac.Env,
		Args:     ac.Args,
		delay:    time.Second,
	}, nil
}

func (s *Supervisor) writeRuntimeConfig(app *App) error {
	if err := os.MkdirAll(app.StateDir, 0o755); err != nil {
		return err
	}
	configPath := filepath.Join(app.StateDir, "runtime-config.toml")
	body := fmt.Sprintf(`[key_value_store.default]
type = "spin"
path = %s

[sqlite_database.default]
type = "spin"
path = %s
`, strconv.Quote(filepath.Join(app.StateDir, "kv.db")), strconv.Quote(filepath.Join(app.StateDir, "sqlite.db")))
	return os.WriteFile(configPath, []byte(body), 0o644)
}

func (s *Supervisor) spinArgs(app *App) []string {
	args := []string{
		"up",
		"-f", app.Dir,
		"--listen", fmt.Sprintf("127.0.0.1:%d", app.Port),
		"--runtime-config-file", filepath.Join(app.StateDir, "runtime-config.toml"),
		"--state-dir", app.StateDir,
		"--log-dir", filepath.Join(s.cfg.logDir, app.Name),
		"--cache-dir", s.cfg.cacheDir,
	}

	keys := make([]string, 0, len(app.Env))
	for key := range app.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-e", key+"="+app.Env[key])
	}
	return append(args, app.Args...)
}

// awaitApp polls the app's health path until it answers, the process exits, the supervisor stops, or the timeout elapses.
// Spin compiles the Wasm component on first start, which can take a minute or more on a small CPU quota.
func (s *Supervisor) awaitApp(app *App, timeout time.Duration) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.Port, app.Health)
	deadline := time.Now().Add(timeout)

	for {
		if s.stopping.Load() {
			return fmt.Errorf("supervisor is shutting down")
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}

		app.mu.Lock()
		running := app.cmd != nil
		app.mu.Unlock()
		if !running {
			return fmt.Errorf("process exited before it answered %s", url)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("no response on %s within %s", url, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *Supervisor) waitReady(app *App) {
	if err := s.awaitApp(app, 10*time.Second); err != nil {
		log.Printf("app %s: not ready: %v", app.Name, err)
		return
	}
	log.Printf("app %s: responding on http://127.0.0.1:%d%s", app.Name, app.Port, app.Health)
}

func (s *Supervisor) retireApp(app *App) {
	app.mu.Lock()
	app.retired = true
	app.mu.Unlock()
}

func (s *Supervisor) unretireApp(app *App) {
	app.mu.Lock()
	app.retired = false
	app.mu.Unlock()
}

func (s *Supervisor) startApp(app *App) {
	s.startAppWait(app, true)
}

// startAppWait starts the app's process and, unless a caller waits for readiness itself, logs when it answers.
func (s *Supervisor) startAppWait(app *App, watchReady bool) {
	app.mu.Lock()
	if app.cmd != nil || app.retired || s.stopping.Load() {
		app.mu.Unlock()
		return
	}

	retry := func() {
		time.AfterFunc(5*time.Second, func() {
			if !s.stopping.Load() {
				s.startApp(app)
			}
		})
	}

	if err := s.writeRuntimeConfig(app); err != nil {
		app.mu.Unlock()
		log.Printf("app %s: write runtime config: %v", app.Name, err)
		retry()
		return
	}

	cmd := exec.Command(s.cfg.spinBin, s.spinArgs(app)...)
	cmd.Env = minimalEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := newPrefixWriter(app.Name)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		app.mu.Unlock()
		log.Printf("app %s: start: %v", app.Name, err)
		retry()
		return
	}

	done := make(chan struct{})
	app.cmd = cmd
	app.done = done
	app.startedAt = time.Now()
	app.mu.Unlock()

	log.Printf("app %s: started pid %d on 127.0.0.1:%d (%s)", app.Name, cmd.Process.Pid, app.Port, app.Host)

	go func() {
		err := cmd.Wait()
		// A surviving trigger child would keep the app's port, so clean up the process group before restarting.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(done)

		app.mu.Lock()
		if app.cmd != cmd {
			app.mu.Unlock()
			return
		}
		app.cmd = nil
		app.done = nil
		ran := time.Since(app.startedAt)
		if ran > 30*time.Second {
			app.delay = time.Second
		} else if app.delay < time.Second {
			app.delay = time.Second
		} else {
			app.delay *= 2
			if app.delay > 30*time.Second {
				app.delay = 30 * time.Second
			}
		}
		delay := app.delay
		retired := app.retired
		app.mu.Unlock()

		if s.stopping.Load() || retired {
			return
		}
		log.Printf("app %s: spin exited (%v) after %s; restarting in %s", app.Name, err, ran.Round(time.Second), delay)
		time.AfterFunc(delay, func() {
			if !s.stopping.Load() {
				s.startApp(app)
			}
		})
	}()

	if watchReady {
		go s.waitReady(app)
	}
}

func (s *Supervisor) stopApp(app *App) {
	app.mu.Lock()
	cmd, done := app.cmd, app.done
	app.cmd, app.done = nil, nil
	app.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	log.Printf("app %s: stopping pid %d", app.Name, cmd.Process.Pid)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-done:
	case <-time.After(s.cfg.stopTimeout):
		log.Printf("app %s: stop timed out, killing pid %d", app.Name, cmd.Process.Pid)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func (s *Supervisor) stopApps() {
	s.appsMu.RLock()
	apps := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.appsMu.RUnlock()

	var wg sync.WaitGroup
	for _, app := range apps {
		wg.Add(1)
		go func(app *App) {
			defer wg.Done()
			s.stopApp(app)
		}(app)
	}
	wg.Wait()
}

func (s *Supervisor) loadApps() error {
	entries, err := os.ReadDir(s.cfg.appsDir)
	if err != nil {
		return err
	}

	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validName(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.cfg.appsDir, name, "spin.toml")); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	hosts := map[string]string{}
	for _, name := range names {
		app, err := s.newApp(name, 0)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		if other, ok := hosts[app.Host]; ok {
			log.Printf("app %s: host %q is already used by %s (skipped)", name, app.Host, other)
			continue
		}
		if s.cfg.deployHost != "" && app.Host == s.cfg.deployHost {
			log.Printf("app %s: host %q is reserved for the deploy API (skipped)", name, app.Host)
			continue
		}
		hosts[app.Host] = name
		s.apps[name] = app
	}

	for _, app := range s.apps {
		s.startApp(app)
	}
	return nil
}

func (s *Supervisor) shutdown() {
	s.stopping.Store(true)

	if s.api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.api.Shutdown(ctx)
		cancel()
	}

	s.stopApps()
	if err := stopCaddy(); err != nil {
		log.Printf("caddy: %v", err)
	}
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := configFromEnv()
	for _, check := range []struct {
		name, value string
	}{
		{"APPS_DOMAIN", cfg.appsDomain},
		{"DEPLOY_HOST", cfg.deployHost},
	} {
		if check.value == "" {
			continue
		}
		if err := validateHost(check.value); err != nil {
			log.Fatalf("%s: %v", check.name, err)
		}
	}

	if err := prepareRuntime(cfg); err != nil {
		log.Fatalf("prepare runtime: %v", err)
	}

	s := &Supervisor{
		cfg:       cfg,
		apps:      map[string]*App{},
		apiExited: make(chan struct{}),
	}

	if err := s.loadApps(); err != nil {
		s.shutdown()
		log.Fatalf("load apps: %v", err)
	}
	if err := s.startAPI(); err != nil {
		s.shutdown()
		log.Fatalf("start API: %v", err)
	}
	if err := s.applyCaddyConfig(); err != nil {
		s.shutdown()
		log.Fatalf("start Caddy: %v", err)
	}

	log.Printf("supervisor ready: %d app(s), Caddy on %s, API on %s", s.appCount(), cfg.listen, cfg.apiListen)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	exit := 0
	select {
	case got := <-sig:
		log.Printf("received %s, shutting down", got)
	case <-s.apiExited:
		log.Printf("API server exited unexpectedly")
		exit = 1
	}

	s.shutdown()
	os.Exit(exit)
}
