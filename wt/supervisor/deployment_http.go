package main

import (
	"net/http"
	"os"
)

func (s *supervisor) handleDeployments(w http.ResponseWriter, _ *http.Request, name string) {
	if s.lookup(name) == nil {
		if s.deployments == nil || !existsPath(s.deployments.appDir(name)) {
			writeError(w, http.StatusNotFound, "app not found")
			return
		}
	}

	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}

	list, err := s.deployments.list(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.lookup(name) == nil && len(list) == 0 {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}

	running := ""

	if a := s.lookup(name); a != nil {
		a.mu.Lock()
		running = a.digest
		a.mu.Unlock()
	}
	// list is newest first, so the first successful record matching the running digest is the current one.
	for i := range list {
		if list[i].Status != "" && list[i].Status != deploymentStatusOK {
			continue
		}

		if running == "" || list[i].Digest != running {
			continue
		}

		list[i].Current = true

		break
	}

	writeJSON(w, http.StatusOK, list)
}

// handleDeploymentInfo serves one retained history entry.
func (s *supervisor) handleDeploymentInfo(w http.ResponseWriter, _ *http.Request, name, id string) {
	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}

	info, err := s.deployments.info(name, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if a := s.lookup(name); a != nil && (info.Status == "" || info.Status == deploymentStatusOK) {
		a.mu.Lock()
		running := a.digest
		a.mu.Unlock()

		info.Current = running != "" && info.Digest == running
	}

	writeJSON(w, http.StatusOK, info)
}

func (s *supervisor) handleRollback(w http.ResponseWriter, r *http.Request, name, id string) {
	defer s.lockApp(name)()

	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}

	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}

	bundle, err := s.deployments.bundle(name, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	dir, err := os.MkdirTemp(s.cfg.appsDir, "."+name+".rollback-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create rollback directory")
		return
	}
	defer os.RemoveAll(dir)

	if err := copyBundle(bundle, dir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cfg, err := readAppConfig(dir, name, s.cfg.domain, s.cfg.tenantMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg.Name = name
	if err := s.hostConflictError(cfg, name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// ?backup=<snapshot> swaps code, database and /data as one operation, with compensation on failure.
	if backup := r.URL.Query().Get("backup"); backup != "" {
		if s.backups == nil {
			writeError(w, http.StatusServiceUnavailable, "backups unavailable")
			return
		}

		next := s.rollbackAtomic(r.Context(), w, name, dir, cfg, backup)
		if next == nil {
			return
		}

		writeJSON(w, http.StatusOK, s.info(next))

		return
	}

	digest := sourceDigest(dir)

	next, mode, err := s.installBundle(w, name, cfg, dir, digest, "rolled back")
	if err != nil {
		s.metrics.observeDeploy(name, "rollback-failed")
		s.recordDeploymentFailure(name, digest, err)

		return
	}

	s.metrics.observeDeploy(name, "rollback")
	s.recordDeployment(name, next.dir)
	w.Header().Set("X-Wt-Wasm-Digest", next.digest)
	w.Header().Set("X-Wt-Deploy-Mode", mode)
	writeJSON(w, http.StatusOK, s.info(next))
}
