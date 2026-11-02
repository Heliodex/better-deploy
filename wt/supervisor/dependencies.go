// App dependencies and explicit maintenance mode.

package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

const maintenanceRetrySeconds = 300

type maintenanceResponse struct {
	Enabled bool `json:"enabled"`
}

type maintenanceRequest struct {
	Enabled bool `json:"enabled"`
}

// dependencyCycleError rejects a new depends_on edge that would make the routing graph cyclic.
func (s *supervisor) dependencyCycleError(name string, deps []string) error {
	visiting := map[string]bool{name: true}
	return s.walkDependencies(name, deps, visiting, []string{name})
}

func (s *supervisor) walkDependencies(name string, deps []string, visiting map[string]bool, path []string) error {
	for _, dep := range deps {
		if dep == name || visiting[dep] {
			return fmt.Errorf("depends_on cycle: %s", strings.Join(append(path, dep), " -> "))
		}
		a := s.lookup(dep)
		if a == nil {
			continue
		}
		a.mu.Lock()
		next := append([]string(nil), a.cfg.DependsOn...)
		a.mu.Unlock()
		visiting[dep] = true
		if err := s.walkDependencies(dep, next, visiting, append(path, dep)); err != nil {
			return err
		}
		delete(visiting, dep)
	}
	return nil
}

// dependenciesReady reports whether every direct dependency is ready, including its own dependencies.
func (s *supervisor) dependenciesReady(a *app) (bool, []string) {
	a.mu.Lock()
	deps := append([]string(nil), a.cfg.DependsOn...)
	a.mu.Unlock()
	var waiting []string
	for _, dep := range deps {
		if !s.appDependencyReady(dep, 0) {
			waiting = append(waiting, dep)
		}
	}
	return len(waiting) == 0, waiting
}

func (s *supervisor) appDependencyReady(name string, depth int) bool {
	if depth > maxDependsOn {
		return false
	}
	a := s.lookup(name)
	if a == nil {
		return false
	}
	a.mu.Lock()
	maintenance := a.cfg.Maintenance
	deps := append([]string(nil), a.cfg.DependsOn...)
	a.mu.Unlock()
	if maintenance {
		return false
	}
	if _, ready := a.servingState(); !ready {
		return false
	}
	for _, dep := range deps {
		if !s.appDependencyReady(dep, depth+1) {
			return false
		}
	}
	return true
}

// handleAppMaintenance reads or toggles an app's maintenance flag without restarting the process.
func (s *supervisor) handleAppMaintenance(w http.ResponseWriter, r *http.Request, name string) {
	defer s.lockApp(name)()
	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		enabled := a.cfg.Maintenance
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, maintenanceResponse{Enabled: enabled})
	case http.MethodPost:
		var request maintenanceRequest
		if !readJSONBody(w, r, maxConfigRequestBytes, &request) {
			return
		}
		a.mu.Lock()
		cfg := a.cfg
		a.mu.Unlock()
		cfg.Maintenance = request.Enabled
		if err := persistAppConfig(a.dir, cfg, s.cfg.tenantMode); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.mu.Lock()
		a.cfg.Maintenance = request.Enabled
		a.mu.Unlock()
		s.broadcast()
		writeJSON(w, http.StatusOK, maintenanceResponse{Enabled: request.Enabled})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveMaintenance replaces every app-host response with a supervisor page while maintenance is enabled.
func (s *supervisor) serveMaintenance(w http.ResponseWriter, r *http.Request, a *app) {
	w.Header().Set("Retry-After", fmt.Sprintf("%d", maintenanceRetrySeconds))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	name := html.EscapeString(a.name)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%[1]s is under maintenance</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1f2933}h1{font-size:1.6rem}p{line-height:1.6}</style></head>
<body><h1>%[1]s is under maintenance</h1><p>The app is temporarily unavailable while its operator makes changes.</p><p>Please try again in a few minutes.</p></body>
</html>
`, name)
}
