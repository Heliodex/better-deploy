package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// viteDev is the frontend dev server configuration for one app.
type viteDev struct {
	appDir, webDir, command, build string
	watch, proxy                   []string
	port                           int
	host, configPath               string
}

// freeLocalPort asks the kernel for an unused loopback port.
func freeLocalPort() (int, error) {
	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
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

	supervisorProxy := dev.SupervisorProxy
	if supervisorProxy == nil {
		supervisorProxy = defaultSupervisorProxy
	}

	proxy = mergeProxyPrefixes(proxy, supervisorProxy)

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

// mergeProxyPrefixes appends groups without duplicating a prefix, preserving the app prefixes first.
func mergeProxyPrefixes(groups ...[]string) []string {
	seen := map[string]struct{}{}

	var out []string

	for _, group := range groups {
		for _, prefix := range group {
			if _, ok := seen[prefix]; ok {
				continue
			}

			seen[prefix] = struct{}{}
			out = append(out, prefix)
		}
	}

	return out
}

// writeViteDevConfig writes a config that keeps the app's own Vite config and adds the app and supervisor proxies.
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

	configPath, err := filepath.Abs(filepath.Join(dev.webDir, ".wt-dev.vite.config.ts"))
	if err != nil {
		return fmt.Errorf("resolve vite dev config: %w", err)
	}

	dev.configPath = configPath
	if err := os.WriteFile(dev.configPath, []byte(content.String()), privateFileMode); err != nil {
		return fmt.Errorf("write vite dev config: %w", err)
	}

	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// startVite runs the web dev command with the generated config in its own process group, so stopping it also stops its children.
func startVite(dev *viteDev) (*exec.Cmd, error) {
	command := dev.command
	if strings.Contains(command, "vite") {
		command += " --config " + shellQuote(dev.configPath)
	} else {
		command += " -- --config " + shellQuote(dev.configPath)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)
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

// ensureWebDeps runs bun install when node_modules is missing.
func ensureWebDeps(webDir string) error {
	info, err := os.Stat(filepath.Join(webDir, "node_modules"))
	if err == nil && info.IsDir() {
		return nil
	}

	fmt.Fprintln(os.Stderr, "dev: installing web dependencies in", webDir)

	cmd := exec.CommandContext(context.Background(), "bun", "install")
	cmd.Dir = webDir
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bun install: %w", err)
	}

	return nil
}
