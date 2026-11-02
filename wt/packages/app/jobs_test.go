package app

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnqueueJob(t *testing.T) {
	var gotAuth, gotPath string
	var got JobRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.UnmarshalRead(r.Body, &got); err != nil {
			t.Errorf("job request: %v", err)
		}
		status := http.StatusCreated
		if got.IdempotencyKey != "" {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		writeTestJSON(w, Job{ID: "job-1", Path: got.Path, Method: got.Method, Status: JobPending})
	}))
	defer server.Close()
	t.Setenv(storageURLEnv, server.URL)
	t.Setenv(storageTokenEnv, "secret")

	runAt := time.Now().Add(time.Minute).UTC()
	job, created, err := EnqueueJob(t.Context(), server.Client(), JobRequest{
		Path:           "/jobs/run",
		Method:         http.MethodPut,
		Body:           `{"value":"one"}`,
		Headers:        map[string]string{"X-Test": "yes"},
		RunAt:          runAt,
		MaxAttempts:    5,
		IdempotencyKey: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || job.ID != "job-1" || job.Status != JobPending {
		t.Errorf("job = %+v created=%v", job, created)
	}
	if gotAuth != "Bearer secret" || gotPath != jobsPath {
		t.Errorf("auth=%q path=%q", gotAuth, gotPath)
	}
	if got.Path != "/jobs/run" || got.Method != http.MethodPut || got.Body != `{"value":"one"}` || got.Headers["X-Test"] != "yes" || got.MaxAttempts != 5 || !got.RunAt.Equal(runAt) {
		t.Errorf("request = %+v", got)
	}

	_, created, err = EnqueueJob(t.Context(), server.Client(), JobRequest{Path: "/jobs/run", IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("an idempotent enqueue should report created=false")
	}
}

func TestListAndJobActions(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.URL.Path == jobsPath {
			writeTestJSON(w, jobsResponse{Jobs: []Job{{ID: "one", Status: JobPending}}})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, jobsPath+"/")
		id = strings.TrimSuffix(id, "/cancel")
		id = strings.TrimSuffix(id, "/retry")
		writeTestJSON(w, Job{ID: id, Status: JobCanceled})
	}))
	defer server.Close()
	t.Setenv(storageURLEnv, server.URL)
	t.Setenv(storageTokenEnv, "secret")

	jobs, err := ListJobs(t.Context(), server.Client(), JobPending)
	if err != nil || len(jobs) != 1 || jobs[0].ID != "one" {
		t.Fatalf("ListJobs = %+v, %v", jobs, err)
	}
	if _, err := GetJob(t.Context(), server.Client(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := CancelJob(t.Context(), server.Client(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := RetryJob(t.Context(), server.Client(), "one"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/jobs?status=pending",
		"GET /v1/jobs/one",
		"POST /v1/jobs/one/cancel",
		"POST /v1/jobs/one/retry",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests = %v, want %v", requests, want)
	}
}

func TestJobRequiresStorageEnv(t *testing.T) {
	t.Setenv(storageURLEnv, "")
	t.Setenv(storageTokenEnv, "")
	if _, _, err := EnqueueJob(t.Context(), http.DefaultClient, JobRequest{Path: "/jobs/run"}); err == nil || !strings.Contains(err.Error(), storageURLEnv) {
		t.Errorf("EnqueueJob error = %v", err)
	}
}
