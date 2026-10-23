package main

import (
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	"github.com/caddyserver/certmagic"
)

func (s *Supervisor) caddyfile() (string, error) {
	s.appsMu.RLock()
	apps := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.appsMu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })

	host, port, err := splitListen(s.cfg.listen)
	if err != nil {
		return "", err
	}

	apiPort := 9090
	if _, port, err := net.SplitHostPort(s.cfg.apiListen); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			apiPort = n
		}
	}

	var b strings.Builder
	b.WriteString("{\n\tadmin off\n\tauto_https off\n}\n\n")
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

	return b.String(), nil
}

// applyCaddyConfig builds the Caddyfile from the current apps and loads it through Caddy's API.
// Caddy runs in this process, so there is no CLI, config file or admin listener to manage.
func (s *Supervisor) applyCaddyConfig() error {
	s.caddyMu.Lock()
	defer s.caddyMu.Unlock()

	configText, err := s.caddyfile()
	if err != nil {
		return err
	}

	caddy.ConfigAutosavePath = filepath.Join(s.cfg.caddyConfigDir, "autosave.json")
	caddy.DefaultStorage = &certmagic.FileStorage{Path: s.cfg.caddyDataDir}

	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		return fmt.Errorf("Caddyfile adapter is not registered")
	}
	jsonConfig, warnings, err := adapter.Adapt([]byte(configText), nil)
	if err != nil {
		return fmt.Errorf("adapt Caddyfile: %w", err)
	}
	for _, warning := range warnings {
		log.Printf("caddy: %s", warning)
	}

	logWriter, logFlags, logPrefix := log.Writer(), log.Flags(), log.Prefix()
	defer func() {
		log.SetOutput(logWriter)
		log.SetFlags(logFlags)
		log.SetPrefix(logPrefix)
	}()
	if err := caddy.Load(jsonConfig, true); err != nil {
		return fmt.Errorf("load Caddy config: %w", err)
	}
	return nil
}

func stopCaddy() error {
	return caddy.Stop()
}
