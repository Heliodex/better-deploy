package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (s *supervisor) appArgs(a *app, source string) []string {
	a.mu.Lock()
	cfg := a.cfg
	epoch := a.wasmEpoch
	a.mu.Unlock()
	args := []string{
		"serve",
		"-Scli",
		"-Sp3",
		"-W", "component-model-async=y",
		"--allow-precompiled",
		"--target", s.cfg.wasmtimeTarget,
		"--addr", fmt.Sprintf("127.0.0.1:%d", a.port),
	}
	if cfg.Network != "none" {
		args = append(args, "-S", "inherit-network")
	}
	if epoch {
		args = append(args, "-W", "epoch-interruption=y")
		if cfg.requestTimeout > 0 {
			args = append(args, "-W", "timeout="+cfg.requestTimeout.String())
		}
	}
	stateDir := filepath.Join(s.cfg.stateDir, a.name)
	args = append(args, "--dir", stateDir+"::/data")
	for _, key := range sortedKeys(cfg.Env) {
		args = append(args, "--env", key+"="+cfg.Env[key])
	}
	if cfg.MemoryMB > 0 {
		args = append(args, "-W", fmt.Sprintf("max-memory-size=%d", cfg.MemoryMB<<20))
	}
	if s.storage != nil {
		args = append(args,
			"--env", "WT_STORAGE_URL="+s.storage.url(),
			"--env", "WT_STORAGE_TOKEN="+s.storage.tokenFor(a.name),
		)
	}
	return append(args, source)
}

func (s *supervisor) waitReady(a *app, done <-chan struct{}) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	a.mu.Lock()
	health := a.cfg.Health
	a.mu.Unlock()
	target := fmt.Sprintf("http://127.0.0.1:%d%s", a.port, health)
	deadline := time.Now().Add(readyTimeout)
	lastFailure := ""

	for {
		select {
		case <-done:
			return fmt.Errorf("wasmtime exited before %s answered", target)
		default:
		}
		resp, err := client.Get(target)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				return nil
			}
			lastFailure = fmt.Sprintf("status %s: %s", resp.Status, strings.TrimSpace(string(body)))
		} else {
			lastFailure = err.Error()
		}
		if time.Now().After(deadline) {
			if lastFailure != "" {
				return fmt.Errorf("no healthy response on %s within %s (last: %s)", target, readyTimeout, lastFailure)
			}
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
	a.healthOK = false
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	s.stopSchedule(a)
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

func (s *supervisor) register(a *app) {
	s.mu.Lock()
	if old := s.apps[a.name]; old != nil {
		delete(s.hosts, old.cfg.Host)
	}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a
	s.mu.Unlock()
	s.broadcast()
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
	delay := max(a.delay, time.Second)
	a.mu.Unlock()
	s.scheduleRestart(a, delay)
}

func (s *supervisor) restart(a *app) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a.mu.Lock()
	if a.retired || s.stopping.Load() || a.cmd != nil {
		a.mu.Unlock()
		return
	}
	dir := a.dir
	requireEpoch := a.cfg.requestTimeout > 0
	a.mu.Unlock()

	cwasm := filepath.Join(dir, "main.cwasm")
	epoch, err := s.ensureArtifact(dir, requireEpoch)
	if err != nil {
		log.Printf("app %s: restart: %v", a.name, err)
		s.scheduleRetry(a)
		return
	}
	a.mu.Lock()
	a.wasmEpoch = epoch
	a.mu.Unlock()
	if err := s.launch(a, cwasm); err != nil {
		log.Printf("app %s: restart: %v", a.name, err)
		s.kill(a)
		s.cancelRestart(a)
		s.scheduleRetry(a)
	}
}

func (s *supervisor) launch(a *app, source string) error {
	a.mu.Lock()
	a.starting = true
	if a.cfg.MaxConcurrent > 0 {
		a.slots = make(chan struct{}, a.cfg.MaxConcurrent)
	} else {
		a.slots = nil
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.starting = false
		a.mu.Unlock()
	}()

	logging := log.New(os.Stderr, "["+a.name+"] ", 0).Writer()
	recent := newLogRing()
	writers := []io.Writer{logging, recent}
	if a.logs != nil {
		writers = append(writers, a.logs)
	}
	out := io.MultiWriter(writers...)
	cmd := exec.Command(s.cfg.wasmtime, s.appArgs(a, source)...)
	cmd.Env = minimalEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Stdout = out
	cmd.Stderr = out
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
		a.healthOK = false
		if err != nil {
			a.lastError = err.Error()
		}
		ran := time.Since(a.started)
		if ran > 30*time.Second {
			a.delay = time.Second
		} else {
			a.delay = min(max(a.delay*2, time.Second), 30*time.Second)
		}
		retired := a.retired
		delay := a.delay
		a.mu.Unlock()
		close(done)
		s.broadcast()

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
		err = withLaunchLog(source, recent, err)
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		return err
	}
	a.proxy = s.newProxy(a)
	a.mu.Lock()
	host := a.cfg.Host
	a.healthOK = true
	a.healthFailures = 0
	a.lastHealthy = time.Now().UTC()
	a.lastError = ""
	a.mu.Unlock()
	log.Printf("app %s started on 127.0.0.1:%d (%s)", a.name, a.port, host)
	s.broadcast()
	return nil
}

func (s *supervisor) spawn(name, dir string, cfg appConfig, port int) (*app, error) {
	if err := os.MkdirAll(filepath.Join(s.cfg.stateDir, name), 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	cwasm := filepath.Join(dir, "main.cwasm")
	epoch, err := s.ensureArtifact(dir, cfg.requestTimeout > 0)
	if err != nil {
		return nil, err
	}
	logs, err := newAppLog(filepath.Join(s.cfg.stateDir, ".logs", name))
	if err != nil {
		return nil, fmt.Errorf("app logs: %w", err)
	}
	a := &app{
		name:      name,
		dir:       dir,
		port:      port,
		cfg:       cfg,
		logs:      logs,
		done:      make(chan struct{}),
		started:   time.Now().UTC(),
		delay:     time.Second,
		wasmEpoch: epoch,
	}
	s.loadResourceHistory(a)
	if err := s.launch(a, cwasm); err != nil {
		s.stop(a)
		return nil, err
	}
	s.startSchedule(a)
	return a, nil
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
