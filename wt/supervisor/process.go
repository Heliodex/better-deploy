package main

import (
	"context"
	"encoding/json/v2"
	"errors"
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

// waitForPort waits for wasmtime to report its serve port and stores it on the app.
func (s *supervisor) waitForPort(a *app, finder *portFinder, done <-chan struct{}, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case port := <-finder.port:
		a.mu.Lock()
		a.port = port
		a.mu.Unlock()

		return nil
	case <-done:
		return errors.New("wasmtime exited before it reported a serve address")
	case <-timer.C:
		return errors.New("wasmtime did not report a serve address within the startup timeout")
	}
}

// waitReady applies the app's startup limit to waitReadyUntil.
func (s *supervisor) waitReady(a *app, done <-chan struct{}) error {
	a.mu.Lock()
	limit := a.cfg.startupLimit()
	a.mu.Unlock()

	return s.waitReadyUntil(a, done, time.Now().Add(limit))
}

// waitReadyUntil polls the health path with exponential backoff until the deadline or process exit.
func (s *supervisor) waitReadyUntil(a *app, done <-chan struct{}, deadline time.Time) error {
	a.mu.Lock()
	health := a.cfg.Health
	port := a.port
	limit := a.cfg.startupLimit()
	a.mu.Unlock()

	target := fmt.Sprintf("http://127.0.0.1:%d%s", port, health)

	var lastFailure string

	wait := 50 * time.Millisecond

	for {
		select {
		case <-done:
			return fmt.Errorf("wasmtime exited before %s answered", target)
		default:
		}

		timeout := min(healthCheckTimeout, max(time.Until(deadline), time.Millisecond))
		ctx, cancel := context.WithTimeout(context.Background(), timeout)

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			return err
		}

		response, err := healthClient.Do(request)

		cancel()

		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 512))

			_ = response.Body.Close()
			if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				return nil
			}

			lastFailure = fmt.Sprintf("status %s: %s", response.Status, strings.TrimSpace(string(body)))
		} else {
			lastFailure = err.Error()
		}

		if time.Now().After(deadline) {
			if lastFailure != "" {
				return fmt.Errorf("no healthy response on %s within %s (last: %s)", target, limit, lastFailure)
			}

			return fmt.Errorf("no response on %s within %s", target, limit)
		}

		time.Sleep(wait)
		wait = min(wait*2, time.Second)
	}
}

// beginRequest tracks a proxied request unless the app is already draining, so new requests cannot extend a drain.
func (a *app) beginRequest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.draining || a.retired {
		return false
	}

	a.requests.Add(1)

	return true
}

// markDraining stops new proxied requests from entering the app before its in-flight requests are drained.
func (a *app) markDraining() {
	a.mu.Lock()
	a.draining = true
	a.ready = false
	a.mu.Unlock()
}

// kill sends SIGTERM and escalates to SIGKILL after the stop timeout.
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

// stop retires the app, cancels its restart timer and closes its isolation resources.
func (s *supervisor) stop(a *app) {
	if a == nil {
		return
	}

	a.mu.Lock()
	a.retired = true
	a.healthOK = false
	a.ready = false
	iso := a.isolation

	a.isolation = nil
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	s.stopSchedule(a)
	s.kill(a)

	if iso == nil {
		iso = &isolationPlan{app: a}
	}

	iso.close(s)
}

// drainAndStop stops routing new requests, waits for in-flight work, then stops the process.
func (s *supervisor) drainAndStop(a *app, action string) {
	if a == nil {
		return
	}

	a.markDraining()

	if !drainApp(a, drainTimeout) {
		log.Printf("%s %s: still handling requests after %s", action, a.name, drainTimeout)
	}

	s.stop(a)
}

// cancelRestart stops a pending restart timer.
func (s *supervisor) cancelRestart(a *app) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

// register installs the app in the routing tables and starts its job queue.
func (s *supervisor) register(a *app) {
	s.mu.Lock()
	if old := s.apps[a.name]; old != nil {
		s.removeAppHostsLocked(old, old.cfg)
	}

	s.removeAppHostsLocked(a, a.cfg)

	s.apps[a.name] = a
	for _, host := range appHosts(a.cfg) {
		s.hosts[host] = a
	}
	s.mu.Unlock()

	if s.jobs != nil {
		if err := s.jobs.start(a.name, a.cfg.JobConcurrency); err != nil {
			log.Printf("jobs %s: %v", a.name, err)
		}
	}

	s.broadcast()
}

// scheduleRestart runs restart after delay unless the app is retired or the supervisor is stopping.
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

// scheduleRetry uses the current backoff delay, with a one-second floor.
func (s *supervisor) scheduleRetry(a *app) {
	a.mu.Lock()
	delay := max(a.delay, time.Second)
	a.mu.Unlock()
	s.scheduleRestart(a, delay)
}

// restart ensures the artifact and launches a new process under the app lock.
func (s *supervisor) restart(a *app) {
	defer s.lockApp(a.name)()

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
	a.digest = sourceDigest(dir)
	a.mu.Unlock()

	if err := s.launch(a, cwasm); err != nil {
		log.Printf("app %s: restart: %v", a.name, err)
		s.kill(a)
		s.cancelRestart(a)
		s.scheduleRetry(a)
	}
}

// spawn creates an app from a bundle and starts its process and schedule.
func (s *supervisor) spawn(name, dir string, cfg appConfig) (*app, error) {
	if err := os.MkdirAll(filepath.Join(s.cfg.stateDir, name), 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	if s.storage != nil {
		s.storage.setQuota(name, int64(cfg.DiskMB)<<20)
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

	logs.setSecrets(s.appRedactionValues(name))
	a := &app{
		name:      name,
		dir:       dir,
		digest:    sourceDigest(dir),
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

// finishExit resets the app after its process exits and schedules a restart unless it is retired or shutting down.
func (s *supervisor) finishExit(a *app, cmd *exec.Cmd, done chan struct{}, exitErr error, message string) {
	a.mu.Lock()
	if a.cmd != cmd {
		a.mu.Unlock()
		close(done)

		return
	}

	a.cmd = nil
	a.exited = true
	a.workPID = 0
	a.healthOK = false

	a.ready = false
	if exitErr != nil {
		a.lastError = exitErr.Error()
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

	if exitErr != nil {
		log.Printf(message, a.name, exitErr)
	}

	if retired || s.stopping.Load() {
		return
	}

	log.Printf("app %s: restarting in %s", a.name, delay)
	s.scheduleRestart(a, delay)
}

// finishRunnerExit runs the same bookkeeping as a directly waited process, but the exit comes from the runner after it reaps the app.
// finishRunnerExit clears the app's namespace root and runs the normal exit bookkeeping.
func (s *supervisor) finishRunnerExit(a *app, cmd *exec.Cmd, done chan struct{}, nsRoot string, exitErr error) {
	if nsRoot != "" {
		_ = os.RemoveAll(nsRoot)
	}

	s.finishExit(a, cmd, done, exitErr, "app %s: exited: %v")
}

// consumeRunnerStream feeds one app's output into its log writers and turns the runner's exit event into the normal restart bookkeeping.
// consumeRunnerStream forwards runner events into logs, PIDs, ports and restart handling.
func (s *supervisor) consumeRunnerStream(a *app, cmd *exec.Cmd, done chan struct{}, stream *runnerStream, out io.Writer, nsRoot string) {
	for event := range stream.events {
		switch event.Op {
		case "output":
			if len(event.Data) > 0 {
				_, _ = out.Write(event.Data)
			}
		case "work":
			a.mu.Lock()
			a.workPID = event.WorkPID
			a.mu.Unlock()
		case "port":
			a.mu.Lock()
			if a.port == 0 {
				a.port = event.Port
			}
			a.mu.Unlock()
		case "exited":
			var exitErr error
			if event.Error != "" {
				exitErr = errors.New(event.Error)
			} else if event.Code != 0 {
				exitErr = fmt.Errorf("exited with code %d", event.Code)
			}

			s.finishRunnerExit(a, cmd, done, nsRoot, exitErr)

			return
		}
	}
}

// adoptApp rebuilds the in-memory app around a process the runner already owns.
// adoptApp rebuilds supervisor state around a process that survived handover in the runner.
func (s *supervisor) adoptApp(name, dir string, cfg appConfig, info runnerAppInfo) (*app, error) {
	if s.storage != nil {
		s.storage.setQuota(name, int64(cfg.DiskMB)<<20)
		s.storage.restoreToken(name, info.StorageToken)
	}

	logs, err := newAppLog(filepath.Join(s.cfg.stateDir, ".logs", name))
	if err != nil {
		return nil, fmt.Errorf("app logs: %w", err)
	}

	logs.setSecrets(s.appRedactionValues(name))

	epoch := false
	if artifact, err := readArtifact(dir); err == nil {
		epoch = artifact.Epoch
	}

	a := &app{
		name:      name,
		dir:       dir,
		digest:    sourceDigest(dir),
		cfg:       cfg,
		logs:      logs,
		done:      make(chan struct{}),
		started:   info.Started,
		delay:     time.Second,
		wasmEpoch: epoch,
		uid:       info.UID,
		port:      info.Port,
		workPID:   info.WorkPID,
		ready:     true,
		healthOK:  true,
	}
	if a.started.IsZero() {
		a.started = time.Now().UTC()
	}

	a.lastHealthy = time.Now().UTC()

	a.nextHealth = time.Now().Add(cfg.probeInterval())
	if cfg.MaxConcurrent > 0 {
		a.slots = make(chan struct{}, cfg.MaxConcurrent)
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: info.PID}}
	a.cmd = cmd
	a.exited = false
	s.loadResourceHistory(a)

	stream := s.runner.Stream(info.ID)

	logging := log.New(os.Stderr, "["+name+"] ", 0).Writer()
	go s.consumeRunnerStream(a, cmd, a.done, stream, io.MultiWriter(a.logs, logging), "")

	a.isolation = &isolationPlan{app: a}
	a.proxy = s.newProxy(a)
	s.startSchedule(a)

	return a, nil
}

// loadApps reads the apps directory, adopts runner processes and starts the rest.
func (s *supervisor) loadApps() error {
	entries, err := os.ReadDir(s.cfg.appsDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(s.cfg.appsDir, 0o755)
	}

	if err != nil {
		return err
	}

	snapshot := map[string]runnerAppInfo{}
	for _, info := range s.runnerSnapshot {
		snapshot[info.Name] = info
	}

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validName(name) {
			continue
		}

		dir := filepath.Join(s.cfg.appsDir, name)
		if info, ok := snapshot[name]; ok && info.Dir != "" {
			dir = info.Dir
		}

		cfg, err := readAppConfig(dir, name, s.cfg.domain, s.cfg.tenantMode)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}

		if info, ok := snapshot[name]; ok && len(info.Config) > 0 {
			var stored appConfig
			if err := json.Unmarshal(info.Config, &stored); err == nil {
				if normalised, err := normaliseAppConfig(stored, name, s.cfg.domain, s.cfg.tenantMode); err == nil {
					cfg = normalised
				}
			}
		}

		if err := s.hostConflictError(cfg, name); err != nil {
			log.Printf("app %s: %v; skipped", name, err)
			continue
		}

		if info, ok := snapshot[name]; ok {
			a, err := s.adoptApp(name, dir, cfg, info)
			delete(snapshot, name)

			if err != nil {
				log.Printf("app %s: adopt: %v (skipped)", name, err)

				_ = s.runner.Stop(info.ID)

				continue
			}

			s.register(a)

			continue
		}

		a, err := s.spawn(name, dir, cfg)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}

		s.register(a)
	}

	for name, info := range snapshot {
		log.Printf("app %s: running without an app directory; stopping it", name)

		_ = s.runner.Stop(info.ID)
	}

	for _, a := range s.allApps() {
		a.mu.Lock()
		name := a.name
		deps := append([]string(nil), a.cfg.DependsOn...)
		a.mu.Unlock()

		if err := s.dependencyCycleError(name, deps); err != nil {
			return fmt.Errorf("app %s: %w", name, err)
		}
	}

	return nil
}
