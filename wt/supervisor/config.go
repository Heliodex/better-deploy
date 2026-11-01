package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const maxConfigRequestBytes = 64 << 10

// appConfigPatch changes only the fields it names. A null env value removes that key.
type appConfigPatch struct {
	Host               *string            `json:"host"`
	Hosts              *[]string          `json:"hosts"`
	Health             *string            `json:"health"`
	Env                map[string]*string `json:"env"`
	MemoryMB           *int               `json:"memory_mb"`
	DiskMB             *int               `json:"disk_mb"`
	CPUMillicores      *int               `json:"cpu_millicores"`
	PidsMax            *int               `json:"pids_max"`
	IOWeight           *int               `json:"io_weight"`
	MaxConcurrent      *int               `json:"max_concurrency"`
	RequestTimeout     *string            `json:"request_timeout"`
	StartupTimeout     *string            `json:"startup_timeout"`
	HealthInterval     *string            `json:"health_interval"`
	HealthFailures     *int               `json:"health_failures"`
	RestartOnUnhealthy *bool              `json:"restart_on_unhealthy"`
	Network            *string            `json:"network"`
	JobConcurrency     *int               `json:"job_concurrency"`
	AllowCallers       *[]string          `json:"allow_callers"`
	Egress             *[]egressRule      `json:"egress"`
	Schedule           *[]scheduleConfig  `json:"schedule"`
}

// appConfigResponse is the runtime config the panel may show. Env values stay hidden; only their keys are listed.
type appConfigResponse struct {
	Name               string           `json:"name"`
	Host               string           `json:"host"`
	Hosts              []string         `json:"hosts"`
	Health             string           `json:"health"`
	EnvKeys            []string         `json:"env_keys"`
	SecretKeys         []string         `json:"secret_keys"`
	MemoryMB           int              `json:"memory_mb"`
	DiskMB             int              `json:"disk_mb"`
	CPUMillicores      int              `json:"cpu_millicores"`
	PidsMax            int              `json:"pids_max"`
	IOWeight           int              `json:"io_weight"`
	MaxConcurrent      int              `json:"max_concurrency"`
	RequestTimeout     string           `json:"request_timeout"`
	StartupTimeout     string           `json:"startup_timeout"`
	HealthInterval     string           `json:"health_interval"`
	HealthFailures     int              `json:"health_failures"`
	RestartOnUnhealthy bool             `json:"restart_on_unhealthy"`
	Network            string           `json:"network"`
	JobConcurrency     int              `json:"job_concurrency"`
	AllowCallers       []string         `json:"allow_callers"`
	Egress             []egressRule     `json:"egress"`
	Schedule           []scheduleConfig `json:"schedule"`
	RestartRequired    bool             `json:"restart_required,omitempty"`
}

// configETag fingerprints the runtime config for conditional reads and writes.
func configETag(cfg appConfig) string {
	data, _ := json.Marshal(cfg)
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.Trim(candidate, `"`) == strings.Trim(etag, `"`) {
			return true
		}
	}
	return false
}

func configResponse(cfg appConfig) appConfigResponse {
	schedule := cfg.Schedule
	if schedule == nil {
		schedule = []scheduleConfig{}
	}
	hosts := cfg.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	callers := cfg.AllowCallers
	if callers == nil {
		callers = []string{}
	}
	egress := cfg.Egress
	if egress == nil {
		egress = []egressRule{}
	}
	return appConfigResponse{
		Name:               cfg.Name,
		Host:               cfg.Host,
		Hosts:              hosts,
		Health:             cfg.Health,
		EnvKeys:            sortedKeys(cfg.Env),
		SecretKeys:         []string{},
		MemoryMB:           cfg.MemoryMB,
		DiskMB:             cfg.DiskMB,
		CPUMillicores:      cfg.CPUMillicores,
		PidsMax:            cfg.PidsMax,
		IOWeight:           cfg.IOWeight,
		MaxConcurrent:      cfg.MaxConcurrent,
		RequestTimeout:     cfg.RequestTimeout,
		StartupTimeout:     cfg.StartupTimeout,
		HealthInterval:     cfg.HealthInterval,
		HealthFailures:     cfg.HealthFailures,
		RestartOnUnhealthy: cfg.RestartOnUnhealthy,
		Network:            cfg.Network,
		JobConcurrency:     cfg.JobConcurrency,
		AllowCallers:       callers,
		Egress:             egress,
		Schedule:           schedule,
	}
}

func applyConfigPatch(cfg appConfig, patch appConfigPatch) appConfig {
	if patch.Host != nil {
		cfg.Host = *patch.Host
	}
	if patch.Hosts != nil {
		cfg.Hosts = append([]string(nil), (*patch.Hosts)...)
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
	if patch.CPUMillicores != nil {
		cfg.CPUMillicores = *patch.CPUMillicores
	}
	if patch.PidsMax != nil {
		cfg.PidsMax = *patch.PidsMax
	}
	if patch.IOWeight != nil {
		cfg.IOWeight = *patch.IOWeight
	}
	if patch.MaxConcurrent != nil {
		cfg.MaxConcurrent = *patch.MaxConcurrent
	}
	if patch.RequestTimeout != nil {
		cfg.RequestTimeout = *patch.RequestTimeout
	}
	if patch.StartupTimeout != nil {
		cfg.StartupTimeout = *patch.StartupTimeout
	}
	if patch.HealthInterval != nil {
		cfg.HealthInterval = *patch.HealthInterval
	}
	if patch.HealthFailures != nil {
		cfg.HealthFailures = *patch.HealthFailures
	}
	if patch.RestartOnUnhealthy != nil {
		cfg.RestartOnUnhealthy = *patch.RestartOnUnhealthy
	}
	if patch.Network != nil {
		cfg.Network = *patch.Network
	}
	if patch.JobConcurrency != nil {
		cfg.JobConcurrency = *patch.JobConcurrency
	}
	if patch.AllowCallers != nil {
		cfg.AllowCallers = append([]string(nil), (*patch.AllowCallers)...)
	}
	if patch.Egress != nil {
		cfg.Egress = append([]egressRule(nil), (*patch.Egress)...)
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

// appConfigPermission keeps env secrets out of world-readable app.json files in untrusted mode.
func appConfigPermission(untrusted bool) os.FileMode {
	if untrusted {
		return 0o600
	}
	return 0o644
}

// persistAppConfig writes app.json with the tenant-appropriate permission. The optional mode keeps
// existing trusted callers working while untrusted PATCHes write the file 0600 from the start.
func persistAppConfig(dir string, cfg appConfig, modes ...string) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	perm := appConfigPermission(tenantModeArg(modes) == tenantUntrusted)
	if err := writeFileAtomicMode(filepath.Join(dir, "app.json"), append(data, '\n'), perm); err != nil {
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
		etag := configETag(cfg)
		w.Header().Set("ETag", etag)
		if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" && etagMatches(ifNoneMatch, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		response := configResponse(cfg)
		response.SecretKeys = s.secrets.keys(name)
		writeJSON(w, http.StatusOK, response)
		return
	}

	defer s.lockApp(name)()
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
	currentETag := configETag(cfg)
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" && !etagMatches(ifMatch, currentETag) {
		writeError(w, http.StatusPreconditionFailed, "config has changed; reload and retry")
		return
	}
	next, err := normalizeAppConfig(applyConfigPatch(cfg, patch), a.name, s.cfg.domain, s.cfg.tenantMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.hostConflictError(next, a.name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if s.cfg.untrusted() && cfg.DiskMB != next.DiskMB && s.quota.enabled() {
		a.mu.Lock()
		uid := a.uid
		a.mu.Unlock()
		// An app that has never launched gets its quota from prepareLaunch instead.
		if uid > 0 {
			if err := s.quota.apply(uid, int64(next.DiskMB)<<20, maxAppEntries); err != nil {
				writeError(w, http.StatusInternalServerError, "apply data quota: "+err.Error())
				return
			}
		}
	}
	if err := persistAppConfig(a.dir, next, s.cfg.tenantMode); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	a.mu.Lock()
	oldConfig := a.cfg
	a.cfg = next
	if cfg.Health != next.Health || cfg.healthInterval != next.healthInterval {
		a.nextHealth = time.Time{}
	}
	a.mu.Unlock()
	s.setAppHostsLocked(a, oldConfig, next)
	s.mu.Unlock()

	if s.storage != nil && cfg.DiskMB != next.DiskMB {
		s.storage.setQuota(a.name, int64(next.DiskMB)<<20)
	}
	if s.jobs != nil && cfg.JobConcurrency != next.JobConcurrency {
		s.jobs.setConcurrency(a.name, next.JobConcurrency)
	}

	restartRequired := !maps.Equal(cfg.Env, next.Env) || cfg.MemoryMB != next.MemoryMB ||
		cfg.DiskMB != next.DiskMB || cfg.CPUMillicores != next.CPUMillicores || cfg.PidsMax != next.PidsMax ||
		cfg.IOWeight != next.IOWeight || cfg.MaxConcurrent != next.MaxConcurrent ||
		cfg.RequestTimeout != next.RequestTimeout || cfg.Network != next.Network ||
		!slices.Equal(cfg.Egress, next.Egress)
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
	w.Header().Set("ETag", configETag(next))
	response := configResponse(next)
	response.SecretKeys = s.secrets.keys(name)
	response.RestartRequired = restartRequired
	writeJSON(w, http.StatusOK, response)
}
