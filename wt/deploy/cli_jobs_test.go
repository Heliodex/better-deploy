package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunJobsEnqueue(t *testing.T) {
	var gotAuth, gotPath string
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.UnmarshalRead(r.Body, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"job-1","path":"/jobs/run","method":"POST","status":"pending","attempts":0,"maxAttempts":2}`))
	}))
	defer server.Close()

	if err := runJobs([]string{
		"--host", server.URL, "--token", "secret", "demo", "enqueue",
		"--path", "/jobs/run",
		"--body", `{"value":"one"}`,
		"--header", "X-Test: yes",
		"--max-attempts", "2",
	}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotPath != "/apps/demo/jobs" {
		t.Errorf("auth=%q path=%q", gotAuth, gotPath)
	}
	if got["path"] != "/jobs/run" || got["body"] != `{"value":"one"}` || got["maxAttempts"] != float64(2) {
		t.Errorf("payload = %+v", got)
	}
	headers, ok := got["headers"].(map[string]any)
	if !ok || headers["X-Test"] != "yes" {
		t.Errorf("headers = %+v", got["headers"])
	}
}

func TestRunJobsList(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"jobs":[{"id":"job-1","path":"/jobs/run","method":"POST","status":"pending","attempts":0,"maxAttempts":3}]}`))
	}))
	defer server.Close()

	if err := runJobs([]string{"--host", server.URL, "demo", "list", "--status", "pending"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/apps/demo/jobs?status=pending" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestRunJobsRejectsInvalidAction(t *testing.T) {
	err := runJobs([]string{"--host", "http://127.0.0.1:1", "demo", "explode"})
	if err == nil || !strings.Contains(err.Error(), "unknown jobs action") {
		t.Errorf("error = %v", err)
	}
}
