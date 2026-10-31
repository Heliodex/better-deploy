package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type deleteAppResponse struct {
	Name   string `json:"name"`
	Purged bool   `json:"purged"`
}

var errNoComponent = errors.New("app has no precompiled component")

// unregister removes an app from the live maps and notifies the panel. Its process and files are left to the caller.
func (s *supervisor) unregister(a *app) {
	s.mu.Lock()
	if s.apps[a.name] == a {
		delete(s.apps, a.name)
	}
	if s.hosts[a.cfg.Host] == a {
		delete(s.hosts, a.cfg.Host)
	}
	s.mu.Unlock()
	s.broadcast()
}

// handleStop drains in-flight requests and stops an app without removing it; its host returns 503 until it is started again.
func (s *supervisor) handleStop(w http.ResponseWriter, r *http.Request, name string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}
	s.drainAndStop(a, "stop")
	writeJSON(w, http.StatusOK, s.info(a))
}

// startStopped launches an app from its existing main.cwasm and starts its schedule.
func (s *supervisor) startStopped(a *app) error {
	a.mu.Lock()
	dir := a.dir
	requireEpoch := a.cfg.requestTimeout > 0
	a.mu.Unlock()
	cwasm := filepath.Join(dir, "main.cwasm")
	epoch, err := s.ensureArtifact(dir, requireEpoch)
	if err != nil {
		return err
	}
	s.stopSchedule(a)
	a.mu.Lock()
	a.retired = false
	a.draining = false
	a.delay = time.Second
	a.wasmEpoch = epoch
	a.mu.Unlock()
	if err := s.launch(a, cwasm); err != nil {
		return err
	}
	s.startSchedule(a)
	a.sampleResources(time.Now())
	return nil
}

// handleStart relaunches a stopped app from its existing main.cwasm.
func (s *supervisor) handleStart(w http.ResponseWriter, r *http.Request, name string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}

	a.mu.Lock()
	running := a.cmd != nil && !a.exited
	a.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "app is already running")
		return
	}
	s.cancelRestart(a)
	if err := s.startStopped(a); err != nil {
		log.Printf("start %s: %v", name, err)
		status := http.StatusBadGateway
		if errors.Is(err, errNoComponent) {
			status = http.StatusInternalServerError
		}
		writeError(w, status, "app did not start: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.info(a))
}

// purgeAppState removes an app's database, snapshots, logs, resource history and /data files.
func (s *supervisor) purgeAppState(name string) error {
	// Hold the backup manager lock so a snapshot cannot start while its database and files disappear.
	if s.backups != nil {
		s.backups.mu.Lock()
		defer s.backups.mu.Unlock()
	}
	if s.storage != nil {
		s.storage.drop(name)
	}
	for _, dir := range []string{
		filepath.Join(s.cfg.stateDir, name),
		filepath.Join(s.cfg.stateDir, ".logs", name),
		filepath.Join(s.cfg.stateDir, ".storage", name),
		filepath.Join(s.cfg.stateDir, ".backups", name),
		filepath.Join(s.cfg.stateDir, ".deployments", name),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	resource := filepath.Join(s.cfg.stateDir, ".resources", name+".json")
	if err := os.Remove(resource); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// handleDelete removes an app's code and process. State is kept unless ?purge=1 is set.
func (s *supervisor) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	a, ok := s.requireApp(w, r, name)
	if !ok {
		return
	}

	purge := false
	if value := r.URL.Query().Get("purge"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "purge must be a boolean")
			return
		}
		purge = parsed
	}

	s.unregister(a)
	s.drainAndStop(a, "delete")
	if a.logs != nil {
		a.logs.close()
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.appsDir, name)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if purge {
		if err := s.purgeAppState(name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, deleteAppResponse{Name: name, Purged: purge})
}
