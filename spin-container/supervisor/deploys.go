package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	deployWait    = 20 * time.Second
	deployHistory = 64
)

const (
	deployStarting = "starting"
	deployReady    = "ready"
	deployFailed   = "failed"
)

// deployInfo is the JSON view of one deploy, including the app's state once it is ready.
type deployInfo struct {
	ID         string     `json:"id"`
	App        string     `json:"app"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	State      *appInfo   `json:"state,omitempty"`
}

type deployRecord struct {
	info deployInfo
	done chan struct{}
}

func validDeployID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func randomDeployID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// newDeploy remembers a deploy so clients can follow it with GET /deploys/<id>.
// Only the most recent deploys are kept; the oldest is dropped from the map.
func (s *Supervisor) newDeploy(id, name string) *deployRecord {
	record := &deployRecord{
		info: deployInfo{
			ID:        id,
			App:       name,
			Status:    deployStarting,
			StartedAt: time.Now().UTC(),
		},
		done: make(chan struct{}),
	}

	s.deploysMu.Lock()
	s.deploys[id] = record
	s.deployOrder = append(s.deployOrder, id)
	for len(s.deployOrder) > deployHistory {
		delete(s.deploys, s.deployOrder[0])
		s.deployOrder = s.deployOrder[1:]
	}
	s.deploysMu.Unlock()

	return record
}

func (s *Supervisor) finishDeploy(record *deployRecord, app *appInfo, err error) {
	now := time.Now().UTC()

	s.deploysMu.Lock()
	if err != nil {
		record.info.Status = deployFailed
		record.info.Error = err.Error()
	} else {
		record.info.Status = deployReady
		record.info.State = app
	}
	record.info.FinishedAt = &now
	close(record.done)
	s.deploysMu.Unlock()
}

func (s *Supervisor) deploySnapshot(id string) (deployInfo, <-chan struct{}, bool) {
	s.deploysMu.Lock()
	defer s.deploysMu.Unlock()

	record := s.deploys[id]
	if record == nil {
		return deployInfo{}, nil, false
	}
	return record.info, record.done, true
}

// handleDeployStatus returns a deploy record, waiting up to deployWait for a running deploy to finish.
// It stays well under proxy request timeouts, so a CLI can follow a slow deploy without SSE.
func (s *Supervisor) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="spin-supervisor"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/deploys/")
	if !validDeployID(id) {
		writeError(w, http.StatusBadRequest, "invalid deploy id")
		return
	}

	info, done, ok := s.deploySnapshot(id)
	if !ok {
		writeError(w, http.StatusNotFound, "deploy not found")
		return
	}

	if info.Status == deployStarting {
		select {
		case <-done:
		case <-time.After(deployWait):
		case <-r.Context().Done():
			return
		}
		info, _, _ = s.deploySnapshot(id)
	}

	writeJSON(w, http.StatusOK, info)
}
