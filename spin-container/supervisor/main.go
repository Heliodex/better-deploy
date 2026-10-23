// Command supervisor runs several Spin applications in one container.
//
// It starts one `spin up` process per application, writes a Caddy config that routes each hostname to the app's loopback port, and keeps each app's key-value store and SQLite database in a directory on the volume.
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
	appsDir, stateDir, logDir, cacheDir, tempDir string

	caddyDataDir, caddyConfigDir, caddyConfig, caddyAdmin string

	spinBin, caddyBin string

	listen, apiListen string

	appsDomain, deployHost, token string

	stopTimeout time.Duration
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

	caddyMu, deployMu                 sync.Mutex
	caddyCmd                          *exec.Cmd
	caddyDone, caddyExited, apiExited chan struct{}

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
		appsDir:        env("SPIN_APPS_DIR", "/var/lib/spin/apps"),
		stateDir:       env("SPIN_STATE_DIR", "/var/lib/spin/state"),
		logDir:         env("SPIN_LOG_DIR", "/var/lib/spin/logs"),
		cacheDir:       env("SPIN_CACHE_DIR", "/var/lib/spin/cache"),
		tempDir:        env("SPIN_TMP_DIR", "/var/lib/spin/tmp"),
		caddyDataDir:   env("CADDY_DATA_DIR", "/var/lib/spin/caddy/data"),
		caddyConfigDir: env("CADDY_CONFIG_DIR", "/var/lib/spin/caddy/config"),
		caddyConfig:    env("CADDY_CONFIG", "/var/lib/spin/caddy/Caddyfile"),
		caddyAdmin:     env("CADDY_ADMIN", "127.0.0.1:2019"),
		spinBin:        env("SPIN_BIN", "spin"),
		caddyBin:       env("CADDY_BIN", "caddy"),
		listen:         env("LISTEN", "0.0.0.0:8080"),
		apiListen:      env("API_LISTEN", "127.0.0.1:9090"),
		appsDomain:     strings.ToLower(strings.TrimSpace(os.Getenv("APPS_DOMAIN"))),
		deployHost:     strings.ToLower(strings.TrimSpace(os.Getenv("DEPLOY_HOST"))),
		token:          os.Getenv("SUPERVISOR_TOKEN"),
		stopTimeout:    time.Duration(envInt("SPIN_STOP_TIMEOUT_SECONDS", 10)) * time.Second,
	}
}

// minimalEnv keeps the process environment small so container-level secrets such as SUPERVISOR_TOKEN are not inherited by Spin or Caddy.
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

func (s *Supervisor) waitReady(app *App) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.Port, app.Health)
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if s.stopping.Load() {
			return
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			log.Printf("app %s: responding on %s", app.Name, url)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("app %s: no response on %s yet (still running)", app.Name, url)
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

	go s.waitReady(app)
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

func (s *Supervisor) writeCaddyfile() error {
	s.caddyMu.Lock()
	defer s.caddyMu.Unlock()

	s.appsMu.RLock()
	apps := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.appsMu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })

	apiPort := 9090
	if _, port, err := net.SplitHostPort(s.cfg.apiListen); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			apiPort = n
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "{\n\tadmin %s\n\tauto_https off\n}\n\n", s.cfg.caddyAdmin)

	host, port, err := splitListen(s.cfg.listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "http://:%s {\n", port)
	if host != "" && host != "0.0.0.0" && host != "::" {
		fmt.Fprintf(&b, "\tbind %s\n", host)
	}
	fmt.Fprintf(&b, "\t@health path /healthz\n\thandle @health {\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n", apiPort)
	if s.cfg.deployHost != "" {
		fmt.Fprintf(&b, "\t@deploy host %s\n\thandle @deploy {\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n", s.cfg.deployHost, apiPort)
	}
	for i, app := range apps {
		fmt.Fprintf(&b, "\t@app%d host %s\n\thandle @app%d {\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n", i, app.Host, i, app.Port)
	}
	b.WriteString("\thandle {\n\t\trespond \"no app for this host\" 404\n\t}\n}\n")

	return os.WriteFile(s.cfg.caddyConfig, []byte(b.String()), 0o644)
}

func (s *Supervisor) startCaddy() error {
	cmd := exec.Command(s.cfg.caddyBin,
		"run",
		"--config", s.cfg.caddyConfig,
		"--adapter", "caddyfile",
		"--watch",
	)
	cmd.Env = minimalEnv(
		"XDG_DATA_HOME="+s.cfg.caddyDataDir,
		"XDG_CONFIG_HOME="+s.cfg.caddyConfigDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := newPrefixWriter("caddy")
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return err
	}
	s.caddyCmd = cmd
	s.caddyDone = make(chan struct{})

	go func() {
		err := cmd.Wait()
		close(s.caddyDone)
		if s.stopping.Load() {
			return
		}
		log.Printf("caddy: %v", err)
		close(s.caddyExited)
	}()

	log.Printf("caddy started pid %d on %s", cmd.Process.Pid, s.cfg.listen)
	return nil
}

func (s *Supervisor) stopCaddy() {
	cmd := s.caddyCmd
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.caddyDone:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-s.caddyDone
	}
}

func (s *Supervisor) shutdown() {
	s.stopping.Store(true)

	if s.api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.api.Shutdown(ctx)
		cancel()
	}

	s.stopApps()
	s.stopCaddy()
}

func main() {
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

	for _, dir := range []string{
		cfg.appsDir, cfg.stateDir, cfg.logDir, cfg.cacheDir,
		cfg.tempDir, cfg.caddyDataDir, cfg.caddyConfigDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create %s: %v", dir, err)
		}
	}

	s := &Supervisor{
		cfg:         cfg,
		apps:        map[string]*App{},
		caddyExited: make(chan struct{}),
		apiExited:   make(chan struct{}),
	}

	if err := s.loadApps(); err != nil {
		s.shutdown()
		log.Fatalf("load apps: %v", err)
	}
	if err := s.writeCaddyfile(); err != nil {
		s.shutdown()
		log.Fatalf("write Caddy config: %v", err)
	}
	if err := s.startAPI(); err != nil {
		s.shutdown()
		log.Fatalf("start API: %v", err)
	}
	if err := s.startCaddy(); err != nil {
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
	case <-s.caddyExited:
		log.Printf("Caddy exited unexpectedly")
		exit = 1
	case <-s.apiExited:
		log.Printf("API server exited unexpectedly")
		exit = 1
	}

	s.shutdown()
	os.Exit(exit)
}
