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
	"slices"
	"strings"
	"syscall"
	"time"
)

// appArgsFor rewrites the bundle, /data and tmp paths for a namespace launch.
// The helper pivots into a root where the bundle is /app and the state dir is /data, so the host paths must not leak into wasmtime's argv.
func (s *supervisor) appArgsFor(a *app, source string, namespaced bool) (args []string, env []string, err error) {
	a.mu.Lock()
	cfg := a.cfg
	epoch := a.wasmEpoch
	a.mu.Unlock()
	args = []string{
		"serve",
		"-Scli",
		"-Sp3",
		"-W", "component-model-async=y",
		"--allow-precompiled",
		"--target", s.cfg.wasmtimeTarget,
		"--addr", serveAddr,
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
	dirSpec := filepath.Join(s.cfg.stateDir, a.name) + "::/data"
	if namespaced {
		dirSpec = "/data::/data"
	}
	args = append(args, "--dir", dirSpec)

	envIndex := map[string]int{}
	setEnv := func(key, value string) {
		if index, ok := envIndex[key]; ok {
			env[index] = key + "=" + value
			return
		}
		envIndex[key] = len(env)
		args = append(args, "--env", key)
		env = append(env, key+"="+value)
	}
	if namespaced {
		// These paths exist inside the minimal root. App config can override them through the loop below.
		setEnv("TMPDIR", "/tmp")
		setEnv("HOME", "/data")
	}
	for _, key := range sortedKeys(cfg.Env) {
		value := cfg.Env[key]
		if name, ok := strings.CutPrefix(value, secretRefPrefix); ok {
			secret, found := s.secrets.get(a.name, name)
			if !found {
				return nil, nil, fmt.Errorf("env %s: secret %q is not set for app %s", key, name, a.name)
			}
			value = secret
		}
		setEnv(key, value)
	}
	if cfg.MemoryMB > 0 {
		args = append(args, "-W", fmt.Sprintf("max-memory-size=%d", cfg.MemoryMB<<20))
	}
	if s.storage != nil {
		token := s.storage.tokenFor(a.name)
		setEnv("WT_STORAGE_URL", s.storage.url())
		setEnv("WT_STORAGE_TOKEN", token)
		setEnv(appTokenEnv, token)
	}
	if s.fetch != nil {
		setEnv(appNameEnv, a.name)
		setEnv(appFetchURLEnv, s.fetch.url())
		apps := s.allApps()
		names := make([]string, 0, len(apps))
		for _, other := range apps {
			if other.name != a.name {
				names = append(names, other.name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			setEnv(appURLEnvName(name), s.fetch.appURL(name))
		}
	}
	if namespaced {
		source = "/app/main.cwasm"
	}
	return append(args, source), env, nil
}

// appArgs builds the wasmtime argv and the process environment separately.
// Only secret names go into argv, so values are not visible in /proc/<pid>/cmdline.
func (s *supervisor) appArgs(a *app, source string) (args []string, env []string, err error) {
	return s.appArgsFor(a, source, false)
}

// waitForPort waits for wasmtime to report the loopback port it bound.
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
		return fmt.Errorf("wasmtime exited before it reported a serve address")
	case <-timer.C:
		return fmt.Errorf("wasmtime did not report a serve address within the startup timeout")
	}
}

func (s *supervisor) waitReady(a *app, done <-chan struct{}) error {
	a.mu.Lock()
	limit := a.cfg.startupLimit()
	a.mu.Unlock()
	return s.waitReadyUntil(a, done, time.Now().Add(limit))
}

func (s *supervisor) waitReadyUntil(a *app, done <-chan struct{}, deadline time.Time) error {
	a.mu.Lock()
	health := a.cfg.Health
	port := a.port
	limit := a.cfg.startupLimit()
	a.mu.Unlock()
	target := fmt.Sprintf("http://127.0.0.1:%d%s", port, health)
	lastFailure := ""
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
	a.ready = false
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	s.stopSchedule(a)
	s.kill(a)
	s.releaseAppIsolation(a)
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

func (s *supervisor) launch(a *app, source string) error {
	a.mu.Lock()
	a.starting = true
	a.port = 0
	cfg := a.cfg
	deadline := time.Now().Add(cfg.startupLimit())
	if cfg.MaxConcurrent > 0 {
		a.slots = make(chan struct{}, cfg.MaxConcurrent)
	} else {
		a.slots = nil
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.starting = false
		a.mu.Unlock()
	}()

	// Resolve env references before creating a cgroup or process, so a missing secret aborts cleanly.
	namespaced := s.cfg.appNamespaces
	args, processEnv, err := s.appArgsFor(a, source, namespaced)
	if err != nil {
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		return err
	}
	if a.logs != nil {
		a.logs.setSecrets(s.appRedactionValues(a.name))
	}

	iso, err := s.prepareLaunch(a, cfg)
	if err != nil {
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		return err
	}

	var ns *nsLaunch
	var planEnv string
	if namespaced {
		ns, err = s.prepareNamespaces(a, iso)
		if err != nil {
			s.releaseIsolation(a, iso)
			a.mu.Lock()
			a.lastError = err.Error()
			a.mu.Unlock()
			return err
		}
		iso.nsRoot = ns.plan.Root
		planData, marshalErr := json.Marshal(ns.plan)
		if marshalErr != nil {
			s.releaseIsolation(a, iso)
			return fmt.Errorf("encode namespace plan: %w", marshalErr)
		}
		planEnv = nsPlanEnv + "=" + string(planData)
	}

	finder := newPortFinder()
	logging := log.New(os.Stderr, "["+a.name+"] ", 0).Writer()
	recent := newLogRing()
	writers := []io.Writer{finder, logging, recent}
	if a.logs != nil {
		writers = append(writers, a.logs)
	}
	out := io.MultiWriter(writers...)

	var cmd *exec.Cmd
	var pidReader, goWriter *os.File
	if namespaced {
		cmd = exec.Command("/proc/self/exe", append([]string{nsInitArg}, args...)...)
		cmd.Env = append(stripEnv(mergeEnv(minimalEnv(), processEnv), nsPlanEnv), planEnv)
		cmd.SysProcAttr = namespaceSysProcAttr(ns.hostID, ns.hostGID, ns.plan.ClearGroups)
		var pidWriter, goReader *os.File
		pidReader, pidWriter, err = os.Pipe()
		if err != nil {
			s.releaseIsolation(a, iso)
			return fmt.Errorf("pid pipe: %w", err)
		}
		goReader, goWriter, err = os.Pipe()
		if err != nil {
			_ = pidReader.Close()
			_ = pidWriter.Close()
			s.releaseIsolation(a, iso)
			return fmt.Errorf("start pipe: %w", err)
		}
		cmd.ExtraFiles = []*os.File{pidWriter, goReader}
	} else {
		cmd = exec.Command(s.cfg.wasmtime, args...)
		cmd.Env = mergeEnv(minimalEnv(), processEnv)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
		if credential := appCredential(iso); credential != nil {
			cmd.SysProcAttr.Credential = credential
		}
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		if pidReader != nil {
			_ = pidReader.Close()
		}
		if goWriter != nil {
			_ = goWriter.Close()
		}
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
		s.releaseIsolation(a, iso)
		return fmt.Errorf("start app: %w", err)
	}
	if len(cmd.ExtraFiles) > 0 {
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
		cmd.ExtraFiles = nil
		go watchNamespaceChild(a, cmd, pidReader)
	}

	done := make(chan struct{})
	a.mu.Lock()
	a.cmd = cmd
	a.done = done
	a.exited = false
	a.workPID = 0
	a.started = time.Now().UTC()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()

	nsRoot := ""
	if ns != nil {
		nsRoot = ns.plan.Root
	}
	go func() {
		err := cmd.Wait()
		if nsRoot != "" {
			_ = os.RemoveAll(nsRoot)
		}
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

	// A failed launch after the process started must leave no process, cgroup or firewall rule behind.
	fail := func(err error) error {
		if goWriter != nil {
			_ = goWriter.Close()
			goWriter = nil
		}
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		s.kill(a)
		s.releaseIsolation(a, iso)
		return err
	}
	if s.cfg.untrusted() && !namespaced {
		if err := applyProcessLimits(cmd.Process.Pid, cfg); err != nil {
			return fail(fmt.Errorf("apply rlimits: %w", err))
		}
	}
	if iso.cgroup != "" && s.cgroups != nil {
		if err := s.cgroups.addPID(a.name, cmd.Process.Pid); err != nil {
			return fail(err)
		}
	}
	// The helper waits for this byte before it starts wasmtime, so the runtime inherits the cgroup at fork time.
	if goWriter != nil {
		if _, err := goWriter.Write([]byte{1}); err != nil {
			return fail(fmt.Errorf("release app start: %w", err))
		}
		_ = goWriter.Close()
		goWriter = nil
	}
	if err := s.waitForPort(a, finder, done, deadline); err != nil {
		return fail(withLaunchLog(source, recent, err))
	}
	if cfg.Network == "storage" {
		if s.cfg.untrusted() {
			// Rules must be in place before readiness and before the proxy routes traffic.
			if err := s.applyEgress(a); err != nil {
				return fail(fmt.Errorf("egress rules: %w", err))
			}
		} else {
			log.Printf("app %s: network storage in trusted mode has no per-app uid, so inherit-network stays unrestricted", a.name)
		}
	}
	// Run the explicit migration phase before readiness, so a slow schema change is not bounded by startup_timeout.
	if cfg.Network != "none" {
		a.mu.Lock()
		port := a.port
		a.mu.Unlock()
		if err := s.migrateApp(a, port, done); err != nil {
			if errors.Is(err, errNoMigrateEndpoint) {
				log.Printf("app %s: no %s endpoint; using health readiness", a.name, migratePath)
			} else {
				return fail(withLaunchLog(source, recent, fmt.Errorf("storage migration: %w", err)))
			}
		}
	}
	if err := s.waitReady(a, done); err != nil {
		return fail(withLaunchLog(source, recent, err))
	}
	proxy := s.newProxy(a)
	a.mu.Lock()
	host := a.cfg.Host
	a.proxy = proxy
	a.healthOK = true
	a.ready = true
	a.healthFailures = 0
	a.nextHealth = time.Now().Add(a.cfg.probeInterval())
	a.lastHealthy = time.Now().UTC()
	a.lastError = ""
	a.mu.Unlock()
	log.Printf("app %s started on 127.0.0.1:%d (%s)", a.name, a.port, host)
	s.broadcast()
	return nil
}

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
		cfg, err := readAppConfig(dir, name, s.cfg.domain, s.cfg.tenantMode)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		if err := s.hostConflictError(cfg, name); err != nil {
			log.Printf("app %s: %v; skipped", name, err)
			continue
		}
		a, err := s.spawn(name, dir, cfg)
		if err != nil {
			log.Printf("app %s: %v (skipped)", name, err)
			continue
		}
		s.register(a)
	}
	return nil
}
