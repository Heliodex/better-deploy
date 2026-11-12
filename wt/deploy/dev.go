package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

type devConfig struct {
	dir, workspace, wasmtime, target, supervisor, appHost string
	port, adminPort                                       int
	skipBuild, skipCompile, watch, vite                   bool
}

// resolveDevConfig loads dev flags, applies WT_DEV_PORT and derives the admin port from the app port.
func resolveDevConfig(args []string) (*devConfig, error) {
	configPath := configArg(args)

	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}

	flags := newFlagSet("dev")
	dir := flags.String("dir", ".", "app directory containing app.json")
	workspace := flags.String("workspace", "", "directory whose child apps are all served together")
	port := flags.Int("port", 0, "loopback port for the dev supervisor (default 8080)")
	adminPort := flags.Int("admin-port", 0, "separate admin port (default app port + 1)")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME"), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	target := flags.String("target", firstNonEmpty(env("WT_WASMTIME_TARGET"), fileCfg.Target), "wasmtime compile target (default: match the local wasmtime)")
	supervisor := flags.String("supervisor", env("WT_SUPERVISOR"), "supervisor binary (default: PATH, then build from ../../supervisor)")
	appHostDefault := fileCfg.AppHost
	appHost := flags.String("app-host", appHostDefault, "override the app host (default: <app>.localhost)")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build commands")
	skipCompile := flags.Bool("skip-compile", false, "reuse an existing main.cwasm")
	watch := flags.Bool("watch", true, "rebuild and redeploy the component when its inputs change")

	vite := flags.Bool("vite", true, "run the SvelteKit dev server with HMR when a web directory exists")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	resolved := *port
	if resolved == 0 {
		resolved = 8080

		if value := strings.TrimSpace(os.Getenv("WT_DEV_PORT")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 65535 {
				return nil, fmt.Errorf("invalid WT_DEV_PORT %q", value)
			}

			resolved = parsed
		}
	}

	resolvedAdmin := *adminPort
	if resolvedAdmin == 0 {
		resolvedAdmin = resolved + 1
	}

	if resolvedAdmin < 1 || resolvedAdmin > 65535 || resolvedAdmin == resolved {
		return nil, errors.New("admin port must be between 1 and 65535 and differ from the app port")
	}

	if *workspace != "" && *appHost != "" {
		return nil, errors.New("--app-host is only used with a single --dir; workspace apps use <name>.localhost")
	}

	return &devConfig{
		dir:         *dir,
		workspace:   *workspace,
		port:        resolved,
		adminPort:   resolvedAdmin,
		wasmtime:    *wasmtime,
		target:      *target,
		supervisor:  *supervisor,
		appHost:     *appHost,
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
		watch:       *watch,
		vite:        *vite,
	}, nil
}

func dirHasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// workspaceAppDirs lists the immediate child directories that contain app.json, sorted by path.
func workspaceAppDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	dirs := []string{}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if info, err := os.Stat(filepath.Join(path, "app.json")); err == nil && info.Mode().IsRegular() {
			dirs = append(dirs, path)
		}
	}

	slices.Sort(dirs)

	if len(dirs) == 0 {
		return nil, fmt.Errorf("workspace %s has no child app directories with app.json", dir)
	}

	return dirs, nil
}

// resolveDevDirs returns the app directories to serve and rejects duplicate app names.
func resolveDevDirs(cfg *devConfig) ([]string, error) {
	dirs := []string{cfg.dir}
	if cfg.workspace != "" {
		workspaceDirs, err := workspaceAppDirs(cfg.workspace)
		if err != nil {
			return nil, err
		}

		dirs = workspaceDirs
	}

	seen := map[string]struct{}{}

	for _, dir := range dirs {
		m, err := readManifest(dir)
		if err != nil {
			return nil, err
		}

		if _, ok := seen[m.Name]; ok {
			return nil, fmt.Errorf("workspace has two apps named %q", m.Name)
		}

		seen[m.Name] = struct{}{}
	}

	return dirs, nil
}

// resolveDevVite resolves the frontend dev server for a single app and explains when workspace mode ignores --vite.
func resolveDevVite(cfg *devConfig, dirs []string) (*viteDev, error) {
	if !cfg.vite {
		return nil, nil //nolint:nilnil // nil vite means Vite is disabled.
	}

	if len(dirs) > 1 {
		fmt.Fprintln(os.Stderr, "dev: --vite is single-app; workspace mode uses the component rebuild loop")
		return nil, nil //nolint:nilnil // Workspace mode uses the component rebuild loop.
	}

	m, err := readManifest(dirs[0])
	if err != nil {
		return nil, err
	}

	host := cfg.appHost
	if host == "" {
		host = m.Name + ".localhost"
	}

	vite, _, err := resolveViteDev(dirs[0], m, host)
	if err != nil {
		return nil, err
	}

	if vite == nil {
		return nil, nil //nolint:nilnil // A nil result means the app has no Vite dev server.
	}

	fmt.Fprintf(os.Stderr, "dev: vite frontend in %s on 127.0.0.1:%d\n", vite.webDir, vite.port)

	if err := ensureWebDeps(vite.webDir); err != nil {
		return nil, err
	}

	return vite, nil
}

// devWorkDir names the temporary state tree for one dev run. The port is part of the key so two supervisors for the same app do not share runner state.
func devWorkDir(cfg *devConfig, dirs []string) string {
	if len(dirs) == 1 {
		m, _ := readManifest(dirs[0])
		return filepath.Join(os.TempDir(), "wt-dev", fmt.Sprintf("%s-%d", m.Name, cfg.port))
	}

	return filepath.Join(os.TempDir(), "wt-dev", "workspace-"+filepath.Base(filepath.Clean(cfg.workspace)))
}

// overlayDevEnv lets client environment override manifest values in dev, so `FOO=1 wt dev` behaves like the README says. Only keys the manifest already defines are touched, and production deploys stay manifest-pinned.
func overlayDevEnv(m *manifest) {
	if m.Env == nil {
		return
	}

	for key := range m.Env {
		if value, ok := os.LookupEnv(key); ok {
			m.Env[key] = value
		}
	}
}

// deployDevApps builds and uploads every app, returning the configs and the Vite-backed one.
func deployDevApps(cfg *devConfig, dirs []string, deployAddr, target string, vite *viteDev) ([]*deployConfig, *deployConfig, error) {
	configs := make([]*deployConfig, 0, len(dirs))

	var viteDCfg *deployConfig

	for _, dir := range dirs {
		m, err := readManifest(dir)
		if err != nil {
			return nil, nil, err
		}

		host := cfg.appHost
		if cfg.workspace != "" || host == "" {
			host = m.Name + ".localhost"
		}

		dcfg := &deployConfig{
			dir:         dir,
			host:        "http://" + deployAddr,
			token:       "dev",
			wasmtime:    cfg.wasmtime,
			target:      target,
			appHost:     host,
			skipBuild:   cfg.skipBuild,
			skipCompile: cfg.skipCompile,
			overlayEnv:  true,
		}
		if vite != nil && dir == vite.appDir {
			dcfg.buildComponentCommand = vite.build
			dcfg.skipStatic = true
			viteDCfg = dcfg
		}

		m, data, status, err := buildBundle(dcfg)
		if err != nil {
			return nil, nil, err
		}

		printBuildStatus(status)

		if err := upload(dcfg, m, data); err != nil {
			return nil, nil, err
		}

		configs = append(configs, dcfg)

		if vite != nil {
			fmt.Printf("dev: %s API at http://%s:%d/\n", m.Name, host, cfg.port)
		} else {
			fmt.Printf("dev: %s at http://%s:%d/\n", m.Name, host, cfg.port)
		}
	}

	return configs, viteDCfg, nil
}

// runViteDev serves the app through Vite with HMR and optionally watches component inputs.
func runViteDev(cfg *devConfig, vite *viteDev, viteDCfg *deployConfig, stop <-chan os.Signal, done chan struct{}) error {
	if vite.build == "" {
		fmt.Fprintln(os.Stderr, "dev: no dev.build; component rebuilds will run app.json's build_component")
	}

	if err := writeViteDevConfig(vite, cfg.port); err != nil {
		return err
	}
	defer os.Remove(vite.configPath)

	viteCmd, err := startVite(vite)
	if err != nil {
		return err
	}

	defer func() { signalViteGroup(viteCmd, syscall.SIGTERM) }()

	fmt.Printf("dev: vite at http://127.0.0.1:%d/ (HMR)\n", vite.port)

	if cfg.watch {
		go watchComponent(viteDCfg, vite.watch, done)

		fmt.Fprintln(os.Stderr, "dev: watching component inputs")
	}

	viteDone := make(chan error, 1)
	go func() { viteDone <- viteCmd.Wait() }()

	select {
	case <-stop:
		fmt.Fprintln(os.Stderr, "\ndev: stopping")
	case err := <-viteDone:
		if err != nil {
			return fmt.Errorf("vite dev exited: %w", err)
		}
	}

	return nil
}

func runDev(args []string) error {
	cfg, err := resolveDevConfig(args)
	if err != nil {
		return err
	}

	dirs, err := resolveDevDirs(cfg)
	if err != nil {
		return err
	}

	vite, err := resolveDevVite(cfg, dirs)
	if err != nil {
		return err
	}

	workDir := devWorkDir(cfg, dirs)

	appsDir, stateDir, err := prepareDevState(workDir)
	if err != nil {
		return err
	}

	supervisor, err := findSupervisor(cfg, workDir)
	if err != nil {
		return err
	}

	target, err := resolveDevTarget(cfg)
	if err != nil {
		return err
	}

	addr, adminAddr := devAddresses(cfg)

	cmd, err := startDevSupervisor(supervisor, cfg, appsDir, stateDir, addr, adminAddr, target)
	if err != nil {
		return err
	}

	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()

	configs, viteDCfg, err := deployDevApps(cfg, dirs, adminAddr, target, vite)
	if err != nil {
		return err
	}

	fmt.Printf("dev: admin at http://%s/admin/ (token: dev)\n", adminAddr)

	done := make(chan struct{})
	defer close(done)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	if vite != nil && viteDCfg != nil {
		return runViteDev(cfg, vite, viteDCfg, stop, done)
	}

	if cfg.watch {
		for _, dcfg := range configs {
			go watchDev(dcfg, done)
		}

		fmt.Fprintln(os.Stderr, "dev: watching for changes")
	}

	<-stop
	fmt.Fprintln(os.Stderr, "\ndev: stopping")

	return nil
}
