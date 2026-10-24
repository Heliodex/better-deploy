package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	"github.com/caddyserver/certmagic"
)

func proxyHandler(dial string, warnings *[]caddyconfig.Warning) json.RawMessage {
	return caddyconfig.JSONModuleObject(reverseproxy.Handler{
		Upstreams: reverseproxy.UpstreamPool{{Dial: dial}},
	}, "handler", "reverse_proxy", warnings)
}

func staticNotFoundHandler(warnings *[]caddyconfig.Warning) json.RawMessage {
	return caddyconfig.JSONModuleObject(caddyhttp.StaticResponse{
		StatusCode: "404",
		Body:       "no app for this host",
	}, "handler", "static_response", warnings)
}

func hostMatcher(host string, warnings *[]caddyconfig.Warning) caddy.ModuleMap {
	return caddy.ModuleMap{"host": caddyconfig.JSON(caddyhttp.MatchHost{host}, warnings)}
}

func pathMatcher(path string, warnings *[]caddyconfig.Warning) caddy.ModuleMap {
	return caddy.ModuleMap{"path": caddyconfig.JSON(caddyhttp.MatchPath{path}, warnings)}
}

func proxyRoute(matchers caddy.ModuleMap, dial string, warnings *[]caddyconfig.Warning) caddyhttp.Route {
	return caddyhttp.Route{
		MatcherSetsRaw: caddyhttp.RawMatcherSets{matchers},
		HandlersRaw:    []json.RawMessage{proxyHandler(dial, warnings)},
		Terminal:       true,
	}
}

func (s *Supervisor) apiPort() int {
	if _, port, err := net.SplitHostPort(s.cfg.apiListen); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			return n
		}
	}
	return 9090
}

func (s *Supervisor) listenAddr() (string, error) {
	host, port, err := splitListen(s.cfg.listen)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return ":" + port, nil
	}
	return net.JoinHostPort(host, port), nil
}

func (s *Supervisor) sortedApps() []*App {
	s.appsMu.RLock()
	apps := make([]*App, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.appsMu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps
}

func (s *Supervisor) buildConfig() (*caddy.Config, error) {
	listenAddr, err := s.listenAddr()
	if err != nil {
		return nil, err
	}

	var warnings []caddyconfig.Warning
	routes := []caddyhttp.Route{
		proxyRoute(pathMatcher("/healthz", &warnings), fmt.Sprintf("127.0.0.1:%d", s.apiPort()), &warnings),
	}
	if s.cfg.deployHost != "" {
		routes = append(routes, proxyRoute(hostMatcher(s.cfg.deployHost, &warnings), fmt.Sprintf("127.0.0.1:%d", s.apiPort()), &warnings))
	}
	for _, app := range s.sortedApps() {
		routes = append(routes, proxyRoute(hostMatcher(app.Host, &warnings), fmt.Sprintf("127.0.0.1:%d", app.Port), &warnings))
	}
	routes = append(routes, caddyhttp.Route{
		HandlersRaw: []json.RawMessage{staticNotFoundHandler(&warnings)},
	})

	httpApp := &caddyhttp.App{
		Servers: map[string]*caddyhttp.Server{
			"srv0": {
				Listen:    []string{listenAddr},
				Routes:    routes,
				AutoHTTPS: &caddyhttp.AutoHTTPSConfig{Disabled: true},
			},
		},
	}
	httpRaw := caddyconfig.JSON(httpApp, &warnings)
	for _, warning := range warnings {
		log.Printf("caddy: %s", warning)
	}

	return &caddy.Config{
		Admin:   &caddy.AdminConfig{Disabled: true},
		AppsRaw: caddy.ModuleMap{"http": httpRaw},
	}, nil
}

// applyCaddyConfig builds the Caddy config from the current apps and loads it through Caddy's API.
// Caddy runs in this process, so there is no CLI, config file or admin listener to manage.
func (s *Supervisor) applyCaddyConfig() error {
	s.caddyMu.Lock()
	defer s.caddyMu.Unlock()

	cfg, err := s.buildConfig()
	if err != nil {
		return err
	}

	caddy.ConfigAutosavePath = filepath.Join(s.cfg.caddyConfigDir, "autosave.json")
	caddy.DefaultStorage = &certmagic.FileStorage{Path: s.cfg.caddyDataDir}

	jsonConfig, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal Caddy config: %w", err)
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
