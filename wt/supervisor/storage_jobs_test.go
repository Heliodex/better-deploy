package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStorageJobsAPI(t *testing.T) {
	s, manager := jobTestSupervisor(t, 0)
	s.apps["demo"].ready = false
	storage := &storageServer{jobs: manager}

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"path":"/jobs/run","body":"hello","runAt":"2030-01-01T00:00:00Z","maxAttempts":2}`))
	rec := httptest.NewRecorder()
	storage.storageJobs(rec, req, "demo")

	if rec.Code != http.StatusCreated {
		t.Fatalf("enqueue = %d: %s", rec.Code, rec.Body.String())
	}

	var job appJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}

	if job.ID == "" || job.Path != "/jobs/run" || job.Status != jobStatusPending || job.MaxAttempts != 2 {
		t.Errorf("job = %+v", job)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/jobs?status=pending", nil)
	rec = httptest.NewRecorder()
	storage.storageJobs(rec, req, "demo")

	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	var listed jobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}

	if len(listed.Jobs) != 1 || listed.Jobs[0].ID != job.ID {
		t.Errorf("listed = %+v", listed)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/jobs/"+job.ID, nil)
	rec = httptest.NewRecorder()
	storage.storageJob(rec, req, "demo", job.ID, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("show = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/jobs/"+job.ID+"/cancel", nil)
	rec = httptest.NewRecorder()
	storage.storageJob(rec, req, "demo", job.ID, "cancel")

	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	if got, _ := manager.store("demo").get(job.ID); got.Status != jobStatusCanceled {
		t.Errorf("canceled job = %+v", got)
	}
}

func TestStorageJobsUnavailable(t *testing.T) {
	storage := &storageServer{}
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	rec := httptest.NewRecorder()
	storage.storageJobs(rec, req, "demo")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
