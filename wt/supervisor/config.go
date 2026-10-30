package main

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
)

const maxConfigRequestBytes = 64 << 10

// appConfigPatch changes only the fields it names. A null env value removes that key.
type appConfigPatch struct {
	Host           *string            `json:"host"`
	Health         *string            `json:"health"`
	Env            map[string]*string `json:"env"`
	MemoryMB       *int               `json:"memory_mb"`
	DiskMB         *int               `json:"disk_mb"`
	MaxConcurrent  *int               `json:"max_concurrency"`
	RequestTimeout *string            `json:"request_timeout"`
	Network        *string            `json:"network"`
	Schedule       *[]scheduleConfig  `json:"schedule"`
}

// appConfigResponse is the runtime config the panel may show. Env values stay hidden; only their keys are listed.
type appConfigResponse struct {
	Name            string           `json:"name"`
	Host            string           `json:"host"`
	Health          string           `json:"health"`
	EnvKeys         []string         `json:"env_keys"`
	MemoryMB        int              `json:"memory_mb"`
	DiskMB          int              `json:"disk_mb"`
	MaxConcurrent   int              `json:"max_concurrency"`
	RequestTimeout  string           `json:"request_timeout"`
	Network         string           `json:"network"`
	Schedule        []scheduleConfig `json:"schedule"`
	RestartRequired bool             `json:"restart_required,omitempty"`
}

func configResponse(cfg appConfig) appConfigResponse {
	schedule := cfg.Schedule
	if schedule == nil {
		schedule = []scheduleConfig{}
	}
	return appConfigResponse{
		Name:           cfg.Name,
		Host:           cfg.Host,
		Health:         cfg.Health,
		EnvKeys:        sortedKeys(cfg.Env),
		MemoryMB:       cfg.MemoryMB,
		DiskMB:         cfg.DiskMB,
		MaxConcurrent:  cfg.MaxConcurrent,
		RequestTimeout: cfg.RequestTimeout,
		Network:        cfg.Network,
		Schedule:       schedule,
	}
}

func applyConfigPatch(cfg appConfig, patch appConfigPatch) appConfig {
	if patch.Host != nil {
		cfg.Host = *patch.Host
	}
	if patch.Health != nil {
		cfg.Health = *patch.Health
	}
	if patch.MemoryMB != nil {
		cfg.MemoryMB = *patch.MemoryMB
	}
	if patch.DiskMB != nil {
		cfg.DiskMB = *patch.DiskMB
	}
	if patch.MaxConcurrent != nil {
		cfg.MaxConcurrent = *patch.MaxConcurrent
	}
	if patch.RequestTimeout != nil {
		cfg.RequestTimeout = *patch.RequestTimeout
	}
	if patch.Network != nil {
		cfg.Network = *patch.Network
	}
	if patch.Schedule != nil {
		cfg.Schedule = append([]scheduleConfig(nil), (*patch.Schedule)...)
	}
	env := make(map[string]string, len(cfg.Env)+len(patch.Env))
	maps.Copy(env, cfg.Env)
	for key, value := range patch.Env {
		if value == nil {
			delete(env, key)
		} else {
			env[key] = *value
		}
	}
	cfg.Env = env
	return cfg
}

func persistAppConfig(dir string, cfg appConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "app.json"), append(data, '\n')); err != nil {
		return fmt.Errorf("write app.json: %w", err)
	}
	return nil
}

// handleAppConfig reads or updates an app's runtime config. Env and memory changes need a restart to affect the running process; host and schedule changes apply immediately.
func (s *supervisor) handleAppConfig(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Method == http.MethodGet {
		a, ok := s.requireApp(w, r, name)
		if !ok {
			return
		}
		a.mu.Lock()
		cfg := a.cfg
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, configResponse(cfg))
		return
	}

	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxConfigRequestBytes)
	var patch appConfigPatch
	if err := json.UnmarshalRead(r.Body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	next, err := normalizeAppConfig(applyConfigPatch(cfg, patch), a.name, s.cfg.domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Host != cfg.Host && s.hostTaken(next.Host, a.name) {
		writeError(w, http.StatusConflict, fmt.Sprintf("host %q is already used by another app", next.Host))
		return
	}
	if err := persistAppConfig(a.dir, next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	a.mu.Lock()
	oldHost := a.cfg.Host
	a.cfg = next
	a.mu.Unlock()
	if oldHost != next.Host {
		delete(s.hosts, oldHost)
	}
	s.hosts[next.Host] = a
	s.mu.Unlock()

	restartRequired := !maps.Equal(cfg.Env, next.Env) || cfg.MemoryMB != next.MemoryMB ||
		cfg.MaxConcurrent != next.MaxConcurrent || cfg.RequestTimeout != next.RequestTimeout || cfg.Network != next.Network
	if !slices.Equal(cfg.Schedule, next.Schedule) {
		a.mu.Lock()
		running := a.cmd != nil && !a.exited
		a.mu.Unlock()
		if running {
			s.stopSchedule(a)
			s.startSchedule(a)
		}
	}

	s.broadcast()
	response := configResponse(next)
	response.RestartRequired = restartRequired
	writeJSON(w, http.StatusOK, response)
}
