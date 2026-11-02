package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type devConfig struct {
	dir, workspace, wasmtime, target, supervisor, appHost string
	port, adminPort                                       int
	skipBuild, skipCompile, watch, vite                   bool
}

func resolveDevConfig(args []string) (*devConfig, error) {
	configPath := configArg(args)
	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	dir := flags.String("dir", ".", "app directory containing app.json")
	workspace := flags.String("workspace", "", "directory whose child apps are all served together")
	port := flags.Int("port", 0, "loopback port for the dev supervisor (default 8080)")
	adminPort := flags.Int("admin-port", 0, "separate admin port (default app port + 1)")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME", ""), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	target := flags.String("target", firstNonEmpty(env("WT_WASMTIME_TARGET", ""), fileCfg.Target), "wasmtime compile target (default: match the local wasmtime)")
	supervisor := flags.String("supervisor", env("WT_SUPERVISOR", ""), "supervisor binary (default: PATH, then build from ../../supervisor)")
	appHostDefault := fileCfg.AppHost
	appHost := flags.String("app-host", appHostDefault, "override the app host (default: <app>.localhost)")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build command")
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
		return nil, fmt.Errorf("admin port must be between 1 and 65535 and differ from the app port")
	}
	if *workspace != "" && *appHost != "" {
		return nil, fmt.Errorf("--app-host is only used with a single --dir; workspace apps use <name>.localhost")
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

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

func dirHasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// findSupervisor locates the supervisor binary: --supervisor, WT_SUPERVISOR, PATH, then a monorepo sibling build.
func findSupervisor(cfg *devConfig, workDir string) (string, error) {
	if cfg.supervisor != "" {
		if !executable(cfg.supervisor) {
			return "", fmt.Errorf("supervisor %q is not executable", cfg.supervisor)
		}
		return cfg.supervisor, nil
	}
	value := strings.TrimSpace(os.Getenv("WT_SUPERVISOR"))
	if value != "" && !executable(value) {
		return "", fmt.Errorf("supervisor %q is not executable", value)
	}
	if value != "" {
		return value, nil
	}
	if path, err := exec.LookPath("wt-supervisor"); err == nil {
		return path, nil
	}
	src, err := filepath.Abs(filepath.Join(cfg.dir, "..", "..", "supervisor"))
	if err != nil || !dirHasGoMod(src) {
		return "", fmt.Errorf("no wt-supervisor in PATH; pass --supervisor or set WT_SUPERVISOR")
	}
	out := filepath.Join(workDir, "wt-supervisor")
	fmt.Fprintln(os.Stderr, "dev: building supervisor from", src)
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build supervisor: %w", err)
	}
	return out, nil
}

// devTargetProbes are tried in order until the local wasmtime accepts one.
var devTargetProbes = []string{
	"x86_64-unknown-linux-musl",
	"x86_64-unknown-linux-gnu",
	"aarch64-unknown-linux-musl",
	"aarch64-unknown-linux-gnu",
	"x86_64-apple-darwin",
	"aarch64-apple-darwin",
}

// detectTarget asks the local wasmtime which triple it serves, using a tiny precompiled module as a probe.
func detectTarget(wasmtime string) (string, error) {
	dir, err := os.MkdirTemp("", "wt-dev-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "probe.wat")
	if err := os.WriteFile(src, []byte("(module)"), 0o644); err != nil {
		return "", err
	}
	artifact := filepath.Join(dir, "probe.cwasm")
	cmd := exec.Command(wasmtime, "compile", "--target", devTargetProbes[0], src, "-o", artifact)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("wasmtime compile probe: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, target := range devTargetProbes {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := exec.CommandContext(ctx, wasmtime, "serve",
			"--allow-precompiled", "--addr", "127.0.0.1:0", "--target", target, artifact).CombinedOutput()
		cancel()
		if err == nil {
			return target, nil
		}
		if strings.Contains(string(out), "does not match the host") {
			continue
		}
		return target, nil
	}
	return "", fmt.Errorf("no usable target for %s (tried %s); pass --target", wasmtime, strings.Join(devTargetProbes, ", "))
}

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor did not answer %s within %s", url, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// devEnv overrides the supervisor settings for local development, keeping the rest of the environment.
func devEnv(appsDir, stateDir, addr, adminAddr, wasmtime, target string) []string {
	values := map[string]string{
		"WT_APPS_DIR":        appsDir,
		"WT_STATE_DIR":       stateDir,
		"WT_LISTEN":          addr,
		"WT_ADMIN_LISTEN":    adminAddr,
		"WT_APPS_DOMAIN":     "localhost",
		"WT_WASMTIME":        wasmtime,
		"WT_WASMTIME_TARGET": target,
		"WT_TOKEN":           "dev",
		"WT_TLS_CERT":        "",
		"WT_TLS_KEY":         "",
	}
	out := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := values[key]; skip {
			continue
		}
		out = append(out, entry)
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		out = append(out, key+"="+values[key])
	}
	return out
}

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

// viteDev is the frontend dev server configuration for one app.
type viteDev struct {
	appDir, webDir   string
	command, build   string
	watch, proxy     []string
	port             int
	host, configPath string
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %s", listener.Addr())
	}
	return address.Port, nil
}

// resolveViteDev finds the optional web dev loop. A web/package.json is enough; app.json dev fields override the defaults.
func resolveViteDev(appDir string, m manifest, host string) (*viteDev, bool, error) {
	dev := m.Dev
	if dev == nil {
		dev = &appDevConfig{}
	}
	web := dev.Web
	if web == "" {
		web = "web"
	}
	webDir := filepath.Join(appDir, web)
	info, err := os.Stat(filepath.Join(webDir, "package.json"))
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	command := dev.Command
	if strings.TrimSpace(command) == "" {
		command = "bun run dev"
	}
	port := dev.Port
	if port == 0 {
		port, err = freeLocalPort()
		if err != nil {
			return nil, false, fmt.Errorf("find vite port: %w", err)
		}
	}
	proxy := append([]string(nil), dev.Proxy...)
	if len(proxy) == 0 {
		proxy = []string{"/api"}
	}
	return &viteDev{
		appDir:  appDir,
		webDir:  webDir,
		command: command,
		build:   dev.Build,
		watch:   append([]string(nil), dev.Watch...),
		proxy:   proxy,
		port:    port,
		host:    host,
	}, true, nil
}

// writeViteDevConfig writes a config that keeps the app's own Vite config and adds the API proxy.
func writeViteDevConfig(dev *viteDev, supervisorPort int) error {
	baseImport := ""
	for _, name := range []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs"} {
		if info, err := os.Stat(filepath.Join(dev.webDir, name)); err == nil && info.Mode().IsRegular() {
			baseImport = "./" + name
			break
		}
	}
	var content strings.Builder
	if baseImport != "" {
		fmt.Fprintf(&content, "import base from %q\n\n", baseImport)
	} else {
		content.WriteString("const base: any = {}\n\n")
	}
	target := "http://127.0.0.1:" + strconv.Itoa(supervisorPort)
	content.WriteString("const proxy = {\n")
	for _, prefix := range dev.proxy {
		fmt.Fprintf(&content, "  %s: {\n    target: %s,\n    changeOrigin: false,\n    headers: { Host: %s },\n  },\n",
			strconv.Quote(prefix), strconv.Quote(target), strconv.Quote(dev.host))
	}
	content.WriteString("}\n\n")
	content.WriteString("export default async (env: any) => {\n")
	content.WriteString("  const config = (typeof base === \"function\" ? await base(env) : base) ?? {}\n")
	content.WriteString("  return {\n")
	content.WriteString("    ...config,\n")
	content.WriteString("    server: {\n")
	content.WriteString("      ...config.server,\n")
	content.WriteString("      host: \"127.0.0.1\",\n")
	fmt.Fprintf(&content, "      port: %d,\n", dev.port)
	content.WriteString("      strictPort: true,\n")
	content.WriteString("      proxy: {\n")
	content.WriteString("        ...config.server?.proxy,\n")
	content.WriteString("        ...proxy,\n")
	content.WriteString("      },\n")
	content.WriteString("    },\n")
	content.WriteString("  }\n")
	content.WriteString("}\n")

	dev.configPath = filepath.Join(dev.webDir, ".wt-dev.vite.config.ts")
	if err := os.WriteFile(dev.configPath, []byte(content.String()), 0o644); err != nil {
		return fmt.Errorf("write vite dev config: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func startVite(dev *viteDev) (*exec.Cmd, error) {
	command := dev.command
	if strings.Contains(command, "vite") {
		command += " --config " + shellQuote(dev.configPath)
	} else {
		command += " -- --config " + shellQuote(dev.configPath)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dev.webDir
	cmd.Env = append(os.Environ(), "WT_DEV=1")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start vite: %w", err)
	}
	return cmd, nil
}

// signalViteGroup stops the shell and the bun/vite child it started.
func signalViteGroup(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, signal)
}

func ensureWebDeps(webDir string) error {
	info, err := os.Stat(filepath.Join(webDir, "node_modules"))
	if err == nil && info.IsDir() {
		return nil
	}
	fmt.Fprintln(os.Stderr, "dev: installing web dependencies in", webDir)
	cmd := exec.Command("bun", "install")
	cmd.Dir = webDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bun install: %w", err)
	}
	return nil
}

func runDev(args []string) error {
	cfg, err := resolveDevConfig(args)
	if err != nil {
		return err
	}
	dirs := []string{cfg.dir}
	if cfg.workspace != "" {
		dirs, err = workspaceAppDirs(cfg.workspace)
		if err != nil {
			return err
		}
	}
	seen := map[string]struct{}{}
	for _, dir := range dirs {
		m, err := readManifest(dir)
		if err != nil {
			return err
		}
		if _, ok := seen[m.Name]; ok {
			return fmt.Errorf("workspace has two apps named %q", m.Name)
		}
		seen[m.Name] = struct{}{}
	}
	var vite *viteDev
	if cfg.vite && len(dirs) == 1 {
		m, err := readManifest(dirs[0])
		if err != nil {
			return err
		}
		host := cfg.appHost
		if host == "" {
			host = m.Name + ".localhost"
		}
		vite, _, err = resolveViteDev(dirs[0], m, host)
		if err != nil {
			return err
		}
		if vite != nil {
			fmt.Fprintf(os.Stderr, "dev: vite frontend in %s on 127.0.0.1:%d\n", vite.webDir, vite.port)
		}
	}
	if cfg.vite && len(dirs) > 1 {
		fmt.Fprintln(os.Stderr, "dev: --vite is single-app; workspace mode uses the component rebuild loop")
	}
	if vite != nil {
		if err := ensureWebDeps(vite.webDir); err != nil {
			return err
		}
	}

	workName := "app"
	if len(dirs) == 1 {
		m, _ := readManifest(dirs[0])
		workName = m.Name
	} else {
		workName = "workspace-" + filepath.Base(filepath.Clean(cfg.workspace))
	}
	workDir := filepath.Join(os.TempDir(), "wt-dev", workName)
	appsDir := filepath.Join(workDir, "apps")
	stateDir := filepath.Join(workDir, "state")
	for _, dir := range []string{appsDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	supervisor, err := findSupervisor(cfg, workDir)
	if err != nil {
		return err
	}
	target := cfg.target
	if target == "" {
		target, err = detectTarget(cfg.wasmtime)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "dev: using target", target)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.port)
	adminAddr := fmt.Sprintf("127.0.0.1:%d", cfg.adminPort)
	cmd := exec.Command(supervisor)
	cmd.Env = devEnv(appsDir, stateDir, addr, adminAddr, cfg.wasmtime, target)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()
	if err := waitHealthy("http://"+addr+"/healthz", 20*time.Second); err != nil {
		return err
	}

	configs := make([]*deployConfig, 0, len(dirs))
	var viteDCfg *deployConfig
	for _, dir := range dirs {
		m, err := readManifest(dir)
		if err != nil {
			return err
		}
		host := cfg.appHost
		if cfg.workspace != "" || host == "" {
			host = m.Name + ".localhost"
		}
		dcfg := &deployConfig{
			dir:         dir,
			host:        "http://" + addr,
			token:       "dev",
			wasmtime:    cfg.wasmtime,
			target:      target,
			appHost:     host,
			skipBuild:   cfg.skipBuild,
			skipCompile: cfg.skipCompile,
		}
		if vite != nil && dir == vite.appDir {
			dcfg.buildCommand = vite.build
			viteDCfg = dcfg
		}
		m, data, err := buildBundle(dcfg)
		if err != nil {
			return err
		}
		if err := upload(dcfg, m, data); err != nil {
			return err
		}
		configs = append(configs, dcfg)
		if vite != nil {
			fmt.Printf("dev: %s API at http://%s:%d/\n", m.Name, host, cfg.port)
		} else {
			fmt.Printf("dev: %s at http://%s:%d/\n", m.Name, host, cfg.port)
		}
	}
	fmt.Printf("dev: admin at http://%s/admin/ (token: dev)\n", adminAddr)

	done := make(chan struct{})
	defer close(done)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	if vite != nil && viteDCfg != nil {
		if vite.build == "" {
			fmt.Fprintln(os.Stderr, "dev: no dev.build; component rebuilds will run the full app.json build")
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
