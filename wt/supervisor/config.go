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
	IdleTimeout        *string            `json:"idle_timeout"`
	Network            *string            `json:"network"`
	Identity           *string            `json:"identity"`
	Maintenance        *bool              `json:"maintenance"`
	DependsOn          *[]string          `json:"depends_on"`
	JobConcurrency     *int               `json:"job_concurrency"`
	AllowCallers       *[]string          `json:"allow_callers"`
	ServerPaths        *[]string          `json:"server_paths"`
	ServerPages        *[]string          `json:"server_pages"`
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
	IdleTimeout        string           `json:"idle_timeout"`
	Network            string           `json:"network"`
	Identity           string           `json:"identity"`
	Maintenance        bool             `json:"maintenance"`
	DependsOn          []string         `json:"depends_on"`
	JobConcurrency     int              `json:"job_concurrency"`
	AllowCallers       []string         `json:"allow_callers"`
	ServerPaths        []string         `json:"server_paths"`
	ServerPages        []string         `json:"server_pages"`
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

// etagMatches compares an If-Match style header against one ETag, accepting weak quotes and "*".
func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.Trim(candidate, `"`) == strings.Trim(etag, `"`) {
			return true
		}
	}

	return false
}

// configResponse converts the internal config to the public response, using empty slices instead of nil.
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

	serverPaths := cfg.ServerPaths
	if serverPaths == nil {
		serverPaths = []string{}
	}

	serverPages := cfg.ServerPages
	if serverPages == nil {
		serverPages = []string{}
	}

	egress := cfg.Egress
	if egress == nil {
		egress = []egressRule{}
	}

	dependsOn := cfg.DependsOn
	if dependsOn == nil {
		dependsOn = []string{}
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
		IdleTimeout:        cfg.IdleTimeout,
		Network:            cfg.Network,
		Identity:           cfg.Identity,
		Maintenance:        cfg.Maintenance,
		DependsOn:          dependsOn,
		JobConcurrency:     cfg.JobConcurrency,
		AllowCallers:       callers,
		ServerPaths:        serverPaths,
		ServerPages:        serverPages,
		Egress:             egress,
		Schedule:           schedule,
	}
}

// applyValue overwrites a config field only when the patch names it.
func applyValue[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// applySlice replaces a slice field with its own copy when the patch names it.
func applySlice[T any](dst *[]T, src *[]T) {
	if src != nil {
		*dst = append([]T(nil), (*src)...)
	}
}

// mergeConfigEnv applies env additions and deletions to a copy of the current map.
func mergeConfigEnv(current map[string]string, patch map[string]*string) map[string]string {
	env := make(map[string]string, len(current)+len(patch))
	maps.Copy(env, current)

	for key, value := range patch {
		if value == nil {
			delete(env, key)
		} else {
			env[key] = *value
		}
	}

	return env
}

// applyConfigPatch overlays the fields named by a PATCH, leaving omitted values unchanged.
func applyConfigPatch(cfg appConfig, patch appConfigPatch) appConfig {
	applyValue(&cfg.Host, patch.Host)
	applyValue(&cfg.Health, patch.Health)
	applyValue(&cfg.MemoryMB, patch.MemoryMB)
	applyValue(&cfg.DiskMB, patch.DiskMB)
	applyValue(&cfg.CPUMillicores, patch.CPUMillicores)
	applyValue(&cfg.PidsMax, patch.PidsMax)
	applyValue(&cfg.IOWeight, patch.IOWeight)
	applyValue(&cfg.MaxConcurrent, patch.MaxConcurrent)
	applyValue(&cfg.RequestTimeout, patch.RequestTimeout)
	applyValue(&cfg.StartupTimeout, patch.StartupTimeout)
	applyValue(&cfg.HealthInterval, patch.HealthInterval)
	applyValue(&cfg.HealthFailures, patch.HealthFailures)
	applyValue(&cfg.RestartOnUnhealthy, patch.RestartOnUnhealthy)
	applyValue(&cfg.IdleTimeout, patch.IdleTimeout)
	applyValue(&cfg.Network, patch.Network)
	applyValue(&cfg.Identity, patch.Identity)
	applyValue(&cfg.Maintenance, patch.Maintenance)
	applyValue(&cfg.JobConcurrency, patch.JobConcurrency)
	applySlice(&cfg.Hosts, patch.Hosts)
	applySlice(&cfg.DependsOn, patch.DependsOn)
	applySlice(&cfg.AllowCallers, patch.AllowCallers)
	applySlice(&cfg.ServerPaths, patch.ServerPaths)
	applySlice(&cfg.ServerPages, patch.ServerPages)
	applySlice(&cfg.Egress, patch.Egress)
	applySlice(&cfg.Schedule, patch.Schedule)
	cfg.Env = mergeConfigEnv(cfg.Env, patch.Env)

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
// persistAppConfig writes app.json with the tenant-appropriate permission.
func persistAppConfig(dir string, cfg appConfig, mode string) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	perm := appConfigPermission(mode == tenantUntrusted)
	if err := writeFileAtomicMode(filepath.Join(dir, "app.json"), append(data, '\n'), perm); err != nil {
		return fmt.Errorf("write app.json: %w", err)
	}

	return nil
}

// validateConfigPatch normalises a patch and checks host, dependency and quota constraints.
func (s *supervisor) validateConfigPatch(w http.ResponseWriter, a *app, cfg appConfig, patch appConfigPatch) (appConfig, bool) {
	next, err := normaliseAppConfig(applyConfigPatch(cfg, patch), a.name, s.cfg.domain, s.cfg.tenantMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return cfg, false
	}

	if err := s.hostConflictError(next, a.name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return cfg, false
	}

	if err := s.dependencyCycleError(a.name, next.DependsOn); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return cfg, false
	}

	if s.cfg.untrusted() && cfg.DiskMB != next.DiskMB && s.quota.enabled() {
		a.mu.Lock()
		uid := a.uid
		a.mu.Unlock()
		// An app that has never launched gets its quota from prepareLaunch instead.
		if uid > 0 {
			if err := s.quota.apply(uid, int64(next.DiskMB)<<20, maxAppEntries); err != nil {
				writeError(w, http.StatusInternalServerError, "apply data quota: "+err.Error())
				return cfg, false
			}
		}
	}

	return next, true
}

// configRestartRequired reports whether the changes only take effect after the process restarts.
func configRestartRequired(cfg, next appConfig) bool {
	return !maps.Equal(cfg.Env, next.Env) || cfg.MemoryMB != next.MemoryMB ||
		cfg.DiskMB != next.DiskMB || cfg.CPUMillicores != next.CPUMillicores || cfg.PidsMax != next.PidsMax ||
		cfg.IOWeight != next.IOWeight || cfg.MaxConcurrent != next.MaxConcurrent ||
		cfg.RequestTimeout != next.RequestTimeout || cfg.Network != next.Network ||
		!slices.Equal(cfg.Egress, next.Egress)
}

// activateConfigPatch swaps the in-memory config and applies the side effects that do not need a restart.
func (s *supervisor) activateConfigPatch(a *app, cfg, next appConfig) bool {
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

	return configRestartRequired(cfg, next)
}

// handleAppConfigGet serves the current config with its ETag and secret key names.
func (s *supervisor) handleAppConfigGet(w http.ResponseWriter, r *http.Request, name string) {
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
}

// handleAppConfigPatch validates and applies one JSON patch to the app config.
func (s *supervisor) handleAppConfigPatch(w http.ResponseWriter, r *http.Request, name string) {
	defer s.lockApp(name)()

	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}

	var patch appConfigPatch
	if !readJSONBody(w, r, maxConfigRequestBytes, &patch) {
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

	next, ok := s.validateConfigPatch(w, a, cfg, patch)
	if !ok {
		return
	}

	if err := persistAppConfig(a.dir, next, s.cfg.tenantMode); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	restartRequired := s.activateConfigPatch(a, cfg, next)
	w.Header().Set("ETag", configETag(next))
	response := configResponse(next)
	response.SecretKeys = s.secrets.keys(name)
	response.RestartRequired = restartRequired
	writeJSON(w, http.StatusOK, response)
}

// handleAppConfig reads or updates an app's runtime config. Env and memory changes need a restart to affect the running process; host and schedule changes apply immediately.
func (s *supervisor) handleAppConfig(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if r.Method == http.MethodGet {
		s.handleAppConfigGet(w, r, name)
		return
	}

	s.handleAppConfigPatch(w, r, name)
}
