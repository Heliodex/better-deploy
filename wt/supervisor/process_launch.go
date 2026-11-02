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
	"strings"
	"syscall"
	"time"
)

// launchSession carries the resolved inputs shared by the runner and direct launch paths.
type launchSession struct {
	cfg        appConfig
	source     string
	args, env  []string
	iso        *launchIsolation
	ns         *nsLaunch
	planEnv    string
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
	return args
}

// appEnv builds the --env pairs and the process environment, resolving secret references first.
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

	session, err := s.prepareLaunchSession(a, source, cfg, deadline)
	if err != nil {
		return err
	}
	if s.runner != nil {
		return s.launchRunner(a, session)
	}
	return s.launchDirect(a, session)
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
	iso, err := s.prepareLaunch(a, cfg)
	if err != nil {
		a.recordError(err)
		return nil, err
	}
	session.iso = iso
	if !session.namespaced {
		return session, nil
	}
	ns, err := s.prepareNamespaces(a, iso)
	if err != nil {
		s.releaseIsolation(a, iso)
		a.recordError(err)
		return nil, err
	}
	planData, err := json.Marshal(ns.plan)
	if err != nil {
		s.releaseIsolation(a, iso)
		return nil, fmt.Errorf("encode namespace plan: %w", err)
	}
	iso.nsRoot = ns.plan.Root
	session.ns = ns
	session.planEnv = nsPlanEnv + "=" + string(planData)
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
		planData, err := json.Marshal(l.ns.plan)
		if err != nil {
			s.releaseIsolation(a, l.iso)
			return fmt.Errorf("encode namespace plan: %w", err)
		}
		request.Plan = planData
		request.HostID = l.ns.hostID
		request.HostGID = l.ns.hostGID
		request.AllowSetgroups = l.ns.plan.ClearGroups
	}

	stream, started, err := s.runner.Start(request)
	if err != nil {
		s.releaseIsolation(a, l.iso)
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

	nsRoot := ""
	if l.ns != nil {
		nsRoot = l.ns.plan.Root
	}
	go s.consumeRunnerStream(a, cmd, done, stream, out, nsRoot)

	fail := func(err error) error {
		a.recordError(err)
		s.kill(a)
		s.releaseIsolation(a, l.iso)
		return err
	}
	if l.iso.cgroup != "" && s.cgroups != nil {
		if err := s.cgroups.addPID(a.name, cmd.Process.Pid); err != nil {
			return fail(err)
		}
	}
	if l.namespaced {
		if err := s.runner.Go(a.name); err != nil {
			return fail(fmt.Errorf("release app start: %w", err))
		}
	}
	if err := s.awaitAppReady(a, l, done, finder, recent); err != nil {
		return fail(err)
	}
	return nil
}

// launchDirect starts an app as a child process and then waits for readiness.
func (s *supervisor) launchDirect(a *app, l *launchSession) error {
	finder, recent, out := s.launchOutput(a)
	cmd, goWriter, err := s.startDirectProcess(a, l, out)
	if err != nil {
		return err
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
	if l.ns != nil {
		nsRoot = l.ns.plan.Root
	}
	go s.watchDirectExit(a, cmd, done, nsRoot)

	// A failed launch after the process started must leave no process, cgroup or firewall rule behind.
	fail := func(err error) error {
		if goWriter != nil {
			_ = goWriter.Close()
			goWriter = nil
		}
		a.recordError(err)
		s.kill(a)
		s.releaseIsolation(a, l.iso)
		return err
	}
	if s.cfg.untrusted() && !l.namespaced {
		if err := applyProcessLimits(cmd.Process.Pid, l.cfg); err != nil {
			return fail(fmt.Errorf("apply rlimits: %w", err))
		}
	}
	if l.iso.cgroup != "" && s.cgroups != nil {
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
	if err := s.awaitAppReady(a, l, done, finder, recent); err != nil {
		return fail(err)
	}
	return nil
}

// startDirectProcess builds and starts the wasmtime command, using the namespace helper when isolation is on.
func (s *supervisor) startDirectProcess(a *app, l *launchSession, out io.Writer) (cmd *exec.Cmd, goWriter *os.File, err error) {
	var pidReader *os.File
	if l.namespaced {
		cmd = exec.Command("/proc/self/exe", append([]string{nsInitArg}, l.args...)...)
		cmd.Env = append(stripEnv(mergeEnv(minimalEnv(), l.env), nsPlanEnv), l.planEnv)
		cmd.SysProcAttr = namespaceSysProcAttr(l.ns.hostID, l.ns.hostGID, l.ns.plan.ClearGroups)
		var pidWriter, goReader *os.File
		pidReader, pidWriter, err = os.Pipe()
		if err != nil {
			s.releaseIsolation(a, l.iso)
			return nil, nil, fmt.Errorf("pid pipe: %w", err)
		}
		goReader, goWriter, err = os.Pipe()
		if err != nil {
			_ = pidReader.Close()
			_ = pidWriter.Close()
			s.releaseIsolation(a, l.iso)
			return nil, nil, fmt.Errorf("start pipe: %w", err)
		}
		cmd.ExtraFiles = []*os.File{pidWriter, goReader}
	} else {
		cmd = exec.Command(s.cfg.wasmtime, l.args...)
		cmd.Env = mergeEnv(minimalEnv(), l.env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
		if credential := appCredential(l.iso); credential != nil {
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
		s.releaseIsolation(a, l.iso)
		return nil, nil, fmt.Errorf("start app: %w", err)
	}
	if len(cmd.ExtraFiles) > 0 {
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
		cmd.ExtraFiles = nil
		go watchNamespaceChild(a, cmd, pidReader)
	}
	return cmd, goWriter, nil
}

// awaitAppReady waits for the port, applies egress rules, runs migrations and activates the app.
func (s *supervisor) awaitAppReady(a *app, l *launchSession, done <-chan struct{}, finder *portFinder, recent *logRing) error {
	if err := s.waitForPort(a, finder, done, l.deadline); err != nil {
		return withLaunchLog(l.source, recent, err)
	}
	if err := s.applyStorageEgress(a, l.cfg); err != nil {
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

// applyStorageEgress installs per-app rules before readiness and before the proxy routes traffic.
func (s *supervisor) applyStorageEgress(a *app, cfg appConfig) error {
	if cfg.Network != "storage" {
		return nil
	}
	if !s.cfg.untrusted() {
		log.Printf("app %s: network storage in trusted mode has no per-app uid, so inherit-network stays unrestricted", a.name)
		return nil
	}
	if err := s.applyEgress(a); err != nil {
		return fmt.Errorf("egress rules: %w", err)
	}
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
