package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// epochIdleTimeout stands in for "no request timeout". Epoch interruption always needs a deadline, and wasmtime applies the deadline immediately when none is set.
const epochIdleTimeout = 24 * time.Hour

// launchSession carries the resolved inputs shared by the runner and direct launch paths.
type launchSession struct {
	id         string
	cfg        appConfig
	source     string
	args, env  []string
	iso        *isolationPlan
	plan       []byte
	deadline   time.Time
	namespaced bool
}

// recordError stores the last launch error for app info and restart logs.
func (a *app) recordError(err error) {
	a.mu.Lock()
	a.lastError = err.Error()
	a.mu.Unlock()
}

// appServeArgs builds the fixed wasmtime serve flags for one app.
func appServeArgs(target string, cfg appConfig, epoch bool) []string {
	args := []string{
		"serve",
		"-Scli",
		"-Sp3",
		"-W", "component-model-async=y",
		"--allow-precompiled",
		"--target", target,
		"--addr", serveAddr,
		// The Go WASIp3 component cannot safely run concurrent handlers that call wasi:http imports. Let wasmtime use separate instances for concurrent requests instead of multiplexing them into one instance.
		"--max-instance-concurrent-reuse-count=1",
	}
	if cfg.Network != "none" {
		args = append(args, "-S", "inherit-network")
	}

	if epoch {
		args = append(args, "-W", "epoch-interruption=y")

		timeout := cfg.requestTimeout
		if timeout <= 0 {
			timeout = epochIdleTimeout
		}
		// wasmtime parses whole seconds or milliseconds, not Go's "1h0m0s" form.
		args = append(args, "-W", "timeout="+strconv.FormatInt(timeout.Milliseconds(), 10)+"ms")
	}

	return args
}

// appEnv builds the --env pairs and the process environment, resolving secret references first.
// appEnv resolves secret references and adds the storage, identity and app-fetch settings.
func (s *supervisor) appEnv(a *app, cfg appConfig, namespaced bool) (extraArgs, env []string, err error) {
	index := map[string]int{}

	set := func(key, value string) {
		if i, ok := index[key]; ok {
			env[i] = key + "=" + value
			return
		}

		index[key] = len(env)
		extraArgs = append(extraArgs, "--env", key)
		env = append(env, key+"="+value)
	}
	if namespaced {
		// These paths exist inside the minimal root. App config can override them through the loop below.
		set("TMPDIR", "/tmp")
		set("HOME", "/data")
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

		set(key, value)
	}

	if cfg.MemoryMB > 0 {
		extraArgs = append(extraArgs, "-W", fmt.Sprintf("max-memory-size=%d", cfg.MemoryMB<<20))
	}

	if s.storage != nil {
		token := s.storage.tokenFor(a.name)
		set("WT_STORAGE_URL", s.storage.url())
		set("WT_STORAGE_TOKEN", token)
		set(appTokenEnv, token)
	}

	if s.blobs != nil {
		set(appNameEnv, a.name)
		set(blobSecretEnv, s.blobs.secretFor(a.name))
		set(blobPrefixEnv, blobPrefix)
	}

	if cfg.Identity == identityModePlatform {
		if secret := s.identitySecretFor(a.name); secret != "" {
			set(identitySecretEnv, secret)
		}
	}

	if s.fetch != nil {
		set(appNameEnv, a.name)
		set(appFetchURLEnv, s.fetch.url())
		apps := s.allApps()

		names := make([]string, 0, len(apps))
		for _, other := range apps {
			if other.name != a.name {
				names = append(names, other.name)
			}
		}

		slices.Sort(names)

		for _, name := range names {
			set(appURLEnvName(name), s.fetch.appURL(name))
		}
	}

	return extraArgs, env, nil
}

// appArgsFor rewrites the bundle, /data and tmp paths for a namespace launch.
// The helper pivots into a root where the bundle is /app and the state dir is /data, so the host paths must not leak into wasmtime's argv.
// appArgsFor builds the wasmtime argv and environment, rewriting paths for a namespace launch.
func (s *supervisor) appArgsFor(a *app, source string, namespaced bool) (args []string, env []string, err error) {
	a.mu.Lock()
	cfg := a.cfg
	epoch := a.wasmEpoch
	a.mu.Unlock()

	args = appServeArgs(s.cfg.wasmtimeTarget, cfg, epoch)

	dirSpec := filepath.Join(s.cfg.stateDir, a.name) + "::/data"
	if namespaced {
		dirSpec = "/data::/data"
	}

	args = append(args, "--dir", dirSpec)

	extraArgs, env, err := s.appEnv(a, cfg, namespaced)
	if err != nil {
		return nil, nil, err
	}

	args = append(args, extraArgs...)

	if namespaced {
		source = "/app/main.cwasm"
	}

	return append(args, source), env, nil
}

// appArgs builds the wasmtime argv and the process environment separately.
// Only secret names go into argv, so values are not visible in /proc/<pid>/cmdline.
// appArgs builds argv and environment for the direct, un-namespaced launch path.
func (s *supervisor) appArgs(a *app, source string) (args []string, env []string, err error) {
	return s.appArgsFor(a, source, false)
}

// launch starts one app, either through the runner or as a direct child, and returns once it is ready or failed.
func (s *supervisor) launch(a *app, source string) error {
	a.mu.Lock()
	a.starting = true
	a.port = 0
	cfg := a.cfg

	deadline := time.Now().Add(cfg.startupLimit())
	if countBudget(cfg.MaxConcurrent).enabled() {
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

	session, err := s.prepareLaunchSession(a, source, cfg, deadline)
	if err != nil {
		return err
	}

	session.id = a.name + "-" + newRequestID()
	if err := s.launchRunner(a, session); err != nil {
		return err
	}

	a.mu.Lock()
	a.isolation = session.iso
	a.mu.Unlock()

	return nil
}

// prepareLaunchSession resolves the environment, isolation and namespace inputs before any process starts.
func (s *supervisor) prepareLaunchSession(a *app, source string, cfg appConfig, deadline time.Time) (*launchSession, error) {
	session := &launchSession{
		cfg:        cfg,
		source:     source,
		deadline:   deadline,
		namespaced: s.cfg.appNamespaces,
	}

	args, env, err := s.appArgsFor(a, source, session.namespaced)
	if err != nil {
		a.recordError(err)
		return nil, err
	}

	session.args, session.env = args, env

	if a.logs != nil {
		a.logs.setSecrets(s.appRedactionValues(a.name))
	}

	iso, err := s.prepareIsolation(a, cfg)
	if err != nil {
		a.recordError(err)
		return nil, err
	}

	session.iso = iso
	if iso.ns == nil {
		return session, nil
	}

	plan, err := json.Marshal(iso.ns.plan)
	if err != nil {
		iso.close(s)
		return nil, fmt.Errorf("encode namespace plan: %w", err)
	}

	session.plan = plan

	return session, nil
}

// launchOutput builds the writers shared by the port finder, the supervisor log and the app's own log.
func (s *supervisor) launchOutput(a *app) (*portFinder, *logRing, io.Writer) {
	finder := newPortFinder()
	logging := log.New(os.Stderr, "["+a.name+"] ", 0).Writer()
	recent := newLogRing()

	writers := []io.Writer{finder, logging, recent}
	if a.logs != nil {
		writers = append(writers, a.logs)
	}

	return finder, recent, io.MultiWriter(writers...)
}

// launchRunner starts an app through the runner and then waits for readiness.
func (s *supervisor) launchRunner(a *app, l *launchSession) error {
	finder, recent, out := s.launchOutput(a)
	configData, _ := json.Marshal(l.cfg)

	request := runnerStartRequest{
		ID:         l.id,
		Name:       a.name,
		Dir:        a.dir,
		Path:       s.cfg.wasmtime,
		Args:       l.args,
		Env:        mergeEnv(minimalEnv(), l.env),
		Config:     configData,
		Namespaced: l.namespaced,
		UID:        l.iso.uid,
	}
	if s.storage != nil {
		request.StorageToken = s.storage.tokenFor(a.name)
	}

	if l.namespaced {
		request.Plan = l.plan
		request.HostID = l.iso.ns.hostID
		request.HostGID = l.iso.ns.hostGID
		request.AllowSetgroups = l.iso.ns.plan.ClearGroups
	}

	stream, started, err := s.runner.Start(request)
	if err != nil {
		l.iso.close(s)
		a.recordError(err)

		return err
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: started.PID}}
	done := make(chan struct{})

	a.mu.Lock()
	a.cmd = cmd
	a.done = done
	a.exited = false
	a.workPID = started.WorkPID
	a.port = 0

	a.started = time.Now().UTC()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()

	go s.consumeRunnerStream(a, cmd, done, stream, out, l.iso.nsRoot)

	fail := func(err error) error {
		a.recordError(err)
		s.kill(a)
		l.iso.close(s)

		return err
	}
	if err := l.iso.attachProcess(s, cmd.Process.Pid); err != nil {
		return fail(err)
	}

	if l.namespaced {
		if err := s.runner.Go(l.id); err != nil {
			return fail(fmt.Errorf("release app start: %w", err))
		}
	}

	if err := s.awaitAppReady(a, l, done, finder, recent); err != nil {
		return fail(err)
	}

	return nil
}

// awaitAppReady waits for the port, applies egress rules, runs migrations and activates the app.
func (s *supervisor) awaitAppReady(a *app, l *launchSession, done <-chan struct{}, finder *portFinder, recent *logRing) error {
	if err := s.waitForPort(a, finder, done, l.deadline); err != nil {
		return withLaunchLog(l.source, recent, err)
	}

	if err := l.iso.activate(s, l.cfg); err != nil {
		return err
	}

	if err := s.migrateBeforeReady(a, l.cfg, done); err != nil {
		return withLaunchLog(l.source, recent, fmt.Errorf("storage migration: %w", err))
	}

	if err := s.waitReady(a, done); err != nil {
		return withLaunchLog(l.source, recent, err)
	}

	s.markAppReady(a)

	return nil
}

// migrateBeforeReady runs the explicit migration phase before readiness, so a slow schema change is not bounded by startup_timeout.
func (s *supervisor) migrateBeforeReady(a *app, cfg appConfig, done <-chan struct{}) error {
	if cfg.Network == "none" {
		return nil
	}

	a.mu.Lock()
	port := a.port
	a.mu.Unlock()

	err := s.migrateApp(a, port, done)
	if errors.Is(err, errNoMigrateEndpoint) {
		log.Printf("app %s: no %s endpoint; using health readiness", a.name, migratePath)
		return nil
	}

	return err
}

// markAppReady hands the app to the proxy and records it as healthy.
func (s *supervisor) markAppReady(a *app) {
	proxy := s.newProxy(a)
	a.mu.Lock()
	host, port := a.cfg.Host, a.port
	a.proxy = proxy
	a.healthOK = true
	a.ready = true
	a.healthFailures = 0
	a.nextHealth = time.Now().Add(a.cfg.probeInterval())
	a.lastHealthy = time.Now().UTC()
	a.lastError = ""
	a.mu.Unlock()
	log.Printf("app %s started on 127.0.0.1:%d (%s)", a.name, port, host)
	s.broadcast()
}
