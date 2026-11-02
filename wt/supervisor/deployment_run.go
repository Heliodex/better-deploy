// Queued deployment runs: create, follow, cancel and stream one deployment's log.

package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const deploymentLogPoll = 250 * time.Millisecond

// deploymentRun is one active async install. cancel is best-effort: the runner checks it between steps.
type deploymentRun struct {
	app, id string
	cancel  context.CancelFunc
}

func deploymentKey(app, id string) string {
	return app + "/" + id
}

func (s *supervisor) registerDeploymentRun(run *deploymentRun) {
	s.deployRunsMu.Lock()
	defer s.deployRunsMu.Unlock()
	if s.deployRuns == nil {
		s.deployRuns = map[string]*deploymentRun{}
	}
	s.deployRuns[deploymentKey(run.app, run.id)] = run
}

func (s *supervisor) unregisterDeploymentRun(run *deploymentRun) {
	s.deployRunsMu.Lock()
	defer s.deployRunsMu.Unlock()
	delete(s.deployRuns, deploymentKey(run.app, run.id))
}

func (s *supervisor) deploymentRun(app, id string) (*deploymentRun, bool) {
	s.deployRunsMu.Lock()
	defer s.deployRunsMu.Unlock()
	run, ok := s.deployRuns[deploymentKey(app, id)]
	return run, ok
}

func (s *supervisor) deploymentLogf(app, id, format string, args ...any) {
	line := s.redactAppText(app, fmt.Sprintf(format, args...))
	s.deployments.appendLog(app, id, []byte(time.Now().UTC().Format(logTimeFormat)+" "+line+"\n"))
}

func (s *supervisor) deploymentLog(app, id, text string) {
	if text == "" {
		return
	}
	text = s.redactAppText(app, text)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	s.deployments.appendLog(app, id, []byte(text))
}

// finishDeploymentRun writes the terminal status and keeps the failure text bounded and redacted.
func (s *supervisor) finishDeploymentRun(app, id, status string, cause error) deploymentInfo {
	message := ""
	if cause != nil {
		message = s.redactAppText(app, cause.Error())
		if len(message) > deploymentErrorBytes {
			message = message[:deploymentErrorBytes] + "…"
		}
	}
	now := time.Now().UTC()
	info, err := s.deployments.updateRun(app, id, func(info *deploymentInfo) {
		info.Status = status
		info.FinishedAt = now
		info.Error = message
		if status == deploymentStatusOK {
			info.DeployedAt = now
		}
	})
	if err != nil {
		return deploymentInfo{ID: id, Status: status, Error: message}
	}
	return info
}

// deploymentLogWriter captures an install helpers' JSON error writes into the deployment log instead of an HTTP response.
type deploymentLogWriter struct {
	header http.Header
	status int
	log    func(string)
}

func (w *deploymentLogWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *deploymentLogWriter) WriteHeader(status int) {
	w.status = status
}

func (w *deploymentLogWriter) Write(data []byte) (int, error) {
	if w.log != nil {
		w.log(string(data))
	}
	return len(data), nil
}

func (w *deploymentLogWriter) Flush() {}

// handleDeploymentCreate stages the upload synchronously, then runs install and readiness in the background.
func (s *supervisor) handleDeploymentCreate(w http.ResponseWriter, r *http.Request, name string) {
	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}
	if s.rejectOverBudget(w) {
		return
	}
	staged, ok := s.stageDeploy(w, r, name)
	if !ok {
		return
	}
	if value := r.URL.Query().Get("dry_run"); value != "" {
		dryRun, err := strconv.ParseBool(value)
		if err != nil {
			staged.cleanup()
			writeError(w, http.StatusBadRequest, "dry_run must be a boolean")
			return
		}
		if dryRun {
			defer staged.cleanup()
			w.Header().Set("X-WT-Wasm-Digest", staged.digest)
			writeJSON(w, http.StatusOK, deployValidationResponse{Valid: true, Config: configResponse(staged.cfg), Digest: staged.digest})
			return
		}
	}
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, staged.etag) {
		staged.cleanup()
		w.Header().Set("X-WT-Wasm-Digest", staged.digest)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	info, err := s.deployments.createRun(name, staged.digest)
	if err != nil {
		staged.cleanup()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &deploymentRun{app: name, id: info.ID, cancel: cancel}
	s.registerDeploymentRun(run)
	go func() {
		defer s.unregisterDeploymentRun(run)
		defer cancel()
		s.runDeployment(ctx, name, info.ID, staged)
	}()

	w.Header().Set("Location", "/apps/"+name+"/deployments/"+info.ID)
	w.Header().Set("X-WT-Wasm-Digest", staged.digest)
	writeJSON(w, http.StatusAccepted, info)
}

func (s *supervisor) runDeployment(ctx context.Context, name, id string, staged *stagedDeploy) {
	defer staged.cleanup()
	now := time.Now().UTC()
	info, err := s.deployments.updateRun(name, id, func(info *deploymentInfo) {
		if info.Status == deploymentStatusQueued {
			info.Status = deploymentStatusRunning
			info.StartedAt = now
		}
	})
	if err != nil {
		s.deploymentLogf(name, id, "deployment %s could not start: %v", id, err)
		return
	}
	s.deploymentLogf(name, id, "deployment %s started (digest %s)", id, staged.digest)

	if ctx.Err() != nil {
		s.finishDeploymentRun(name, id, deploymentStatusCanceled, nil)
		s.deploymentLogf(name, id, "deployment %s canceled before install", id)
		return
	}

	defer s.lockApp(name)()
	if ctx.Err() != nil {
		s.finishDeploymentRun(name, id, deploymentStatusCanceled, nil)
		s.deploymentLogf(name, id, "deployment %s canceled before install", id)
		return
	}

	writer := &deploymentLogWriter{log: func(text string) { s.deploymentLog(name, id, text) }}
	next, mode, err := s.installBundle(writer, name, staged.cfg, staged.dir, staged.digest, "deployed")
	if err != nil {
		status := deploymentStatusFail
		if ctx.Err() != nil {
			status = deploymentStatusCanceled
		}
		s.metrics.observeDeploy(name, status)
		s.finishDeploymentRun(name, id, status, err)
		s.deploymentLogf(name, id, "deployment %s %s: %v", id, status, err)
		return
	}

	info.Mode = mode
	recorded, err := s.deployments.recordAt(name, id, info, next.dir)
	if err != nil {
		s.metrics.observeDeploy(name, "failed")
		s.finishDeploymentRun(name, id, deploymentStatusFail, err)
		s.deploymentLogf(name, id, "deployment %s could not be recorded: %v", id, err)
		return
	}
	s.metrics.observeDeploy(name, "success")
	s.resetHistoryUsage(name)
	s.deploymentLogf(name, id, "deployment %s succeeded in %s mode", id, recorded.Mode)
}

func (s *supervisor) handleDeploymentRoute(w http.ResponseWriter, r *http.Request, name, rest string) {
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	switch action {
	case "":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleDeploymentInfo(w, r, name, id)
	case "cancel":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleDeploymentCancel(w, r, name, id)
	case "logs":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleDeploymentLogs(w, r, name, id)
	case "logs/stream":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleDeploymentLogStream(w, r, name, id)
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

func (s *supervisor) handleDeploymentCancel(w http.ResponseWriter, _ *http.Request, name, id string) {
	info, err := s.deployments.info(name, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if info.Status != deploymentStatusQueued && info.Status != deploymentStatusRunning {
		writeError(w, http.StatusConflict, "deployment already "+info.Status)
		return
	}
	if run, ok := s.deploymentRun(name, id); ok {
		run.cancel()
	}
	if info.Status == deploymentStatusQueued {
		info = s.finishDeploymentRun(name, id, deploymentStatusCanceled, nil)
		s.deploymentLogf(name, id, "deployment %s canceled while queued", id)
	} else {
		s.deploymentLogf(name, id, "deployment %s cancellation requested", id)
	}
	w.Header().Set("Location", "/apps/"+name+"/deployments/"+id)
	writeJSON(w, http.StatusAccepted, info)
}

func (s *supervisor) handleDeploymentLogs(w http.ResponseWriter, _ *http.Request, name, id string) {
	data, err := s.deployments.readLog(name, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleDeploymentLogStream sends the log so far and then follows it, ending when the deployment reaches a terminal state.
func (s *supervisor) handleDeploymentLogStream(w http.ResponseWriter, r *http.Request, name, id string) {
	if _, err := s.deployments.info(name, id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	offset := int64(0)
	ticker := time.NewTicker(deploymentLogPoll)
	defer ticker.Stop()
	for {
		data, err := s.deployments.readLog(name, id)
		if err == nil && int64(len(data)) > offset {
			for line := range strings.SplitSeq(string(data[offset:]), "\n") {
				if line == "" {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
			offset = int64(len(data))
			flusher.Flush()
		}
		info, err := s.deployments.info(name, id)
		if err == nil && info.Status != deploymentStatusQueued && info.Status != deploymentStatusRunning {
			fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", info.Status)
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
