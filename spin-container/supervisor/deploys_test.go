package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestSupervisor(token string) *Supervisor {
	return &Supervisor{
		cfg:     config{token: token},
		deploys: map[string]*deployRecord{},
	}
}

func TestValidDeployID(t *testing.T) {
	for _, id := range []string{"abc12345", "0123456789abcdef"} {
		if !validDeployID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "short", "ABC12345", "abc-1234", strings.Repeat("a", 65)} {
		if validDeployID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestDeployRecordLifecycle(t *testing.T) {
	s := newTestSupervisor("t")
	record := s.newDeploy("abc12345", "gotime")

	info, done, ok := s.deploySnapshot("abc12345")
	if !ok || info.Status != deployStarting || info.App != "gotime" {
		t.Fatalf("snapshot = %+v, %v", info, ok)
	}
	select {
	case <-done:
		t.Fatal("a starting deploy should not be done")
	default:
	}

	s.finishDeploy(record, &appInfo{Name: "gotime", Status: "running", PID: 42}, nil)
	info, _, ok = s.deploySnapshot("abc12345")
	if !ok || info.Status != deployReady || info.State == nil || info.State.PID != 42 {
		t.Fatalf("snapshot after finish = %+v, %v", info, ok)
	}
	if info.FinishedAt == nil {
		t.Error("finishedAt is nil")
	}
	select {
	case <-record.done:
	default:
		t.Error("a finished deploy should be done")
	}
}

func TestDeployHistoryPruned(t *testing.T) {
	s := newTestSupervisor("t")
	for i := range deployHistory + 1 {
		s.newDeploy(fmt.Sprintf("id%06d", i), "gotime")
	}

	s.deploysMu.Lock()
	defer s.deploysMu.Unlock()
	if len(s.deploys) != deployHistory {
		t.Errorf("kept %d deploys, want %d", len(s.deploys), deployHistory)
	}
	if _, ok := s.deploys["id000000"]; ok {
		t.Error("the oldest deploy should have been dropped")
	}
}

func serveDeployStatus(t *testing.T, s *Supervisor, id, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/deploys/"+id, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleDeployStatus(rec, req)
	return rec
}

func TestHandleDeployStatus(t *testing.T) {
	s := newTestSupervisor("secret")

	if rec := serveDeployStatus(t, s, "abc12345", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token: %d, want 401", rec.Code)
	}
	if rec := serveDeployStatus(t, s, "abc12345", "secret"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deploy: %d, want 404", rec.Code)
	}
	if rec := serveDeployStatus(t, s, "BAD12345", "secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: %d, want 400", rec.Code)
	}
}

func TestHandleDeployStatusWaits(t *testing.T) {
	s := newTestSupervisor("secret")
	record := s.newDeploy("abc12345", "gotime")

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- serveDeployStatus(t, s, "abc12345", "secret")
	}()

	time.Sleep(100 * time.Millisecond)
	s.finishDeploy(record, &appInfo{Name: "gotime", Status: "running", PID: 7}, nil)

	select {
	case rec := <-answered:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var info deployInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info.Status != deployReady || info.State == nil || info.State.PID != 7 {
			t.Errorf("info = %+v", info)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the status handler did not return after the deploy finished")
	}
}

func TestHandleDeployStatusFailed(t *testing.T) {
	s := newTestSupervisor("secret")
	record := s.newDeploy("abc12345", "gotime")
	s.finishDeploy(record, nil, fmt.Errorf("process exited before it answered"))

	rec := serveDeployStatus(t, s, "abc12345", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var info deployInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Status != deployFailed || !strings.Contains(info.Error, "process exited") {
		t.Errorf("info = %+v", info)
	}
}
