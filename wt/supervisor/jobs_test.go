package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func jobTestSupervisor(t *testing.T, port int) (*supervisor, *jobManager) {
	t.Helper()

	s := testSupervisor()
	a := &app{
		name:  "demo",
		cfg:   appConfig{Host: "demo.localhost", JobConcurrency: 1},
		cmd:   &exec.Cmd{},
		ready: true,
		port:  port,
	}
	s.apps["demo"] = a
	s.hosts["demo.localhost"] = a
	manager := newJobManager(t.TempDir(), s)
	s.jobs = manager
	t.Cleanup(manager.close)

	if err := manager.start("demo", 1); err != nil {
		t.Fatal(err)
	}

	return s, manager
}

func waitJob(t *testing.T, store *jobStore, id, status string) appJob {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := store.get(id)
		if ok && job.Status == status {
			return job
		}

		time.Sleep(20 * time.Millisecond)
	}

	job, _ := store.get(id)
	t.Fatalf("job %s did not reach %s: %+v", id, status, job)

	return appJob{}
}

func TestJobQueueRunsAndPersists(t *testing.T) {
	received := make(chan string, 10)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 64)

		count, _ := r.Body.Read(body)
		received <- r.Method + " " + r.Header.Get("X-Test") + " " + string(body[:count])

		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	address, ok := target.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", target.Listener.Addr())
	}

	s, manager := jobTestSupervisor(t, address.Port)

	store := manager.store("demo")

	job, created, err := store.enqueue(jobEnqueueRequest{
		Path:    "/work",
		Body:    `{"value":"one"}`,
		Headers: map[string]string{"X-Test": "yes"},
	})
	if err != nil || !created {
		t.Fatalf("enqueue = %+v, %v, %v", job, created, err)
	}

	duplicate, created, err := store.enqueue(jobEnqueueRequest{Path: "/work", IdempotencyKey: ""})
	if err != nil || !created || duplicate.ID == job.ID {
		t.Errorf("second enqueue = %+v, %v, %v", duplicate, created, err)
	}

	keyed, created, err := store.enqueue(jobEnqueueRequest{Path: "/other", IdempotencyKey: "same"})
	if err != nil || !created {
		t.Fatal(err)
	}

	same, created, err := store.enqueue(jobEnqueueRequest{Path: "/other", IdempotencyKey: "same"})
	if err != nil || created || same.ID != keyed.ID {
		t.Errorf("idempotent enqueue = %+v, %v, %v", same, created, err)
	}

	done := waitJob(t, store, job.ID, jobStatusSucceeded)
	if done.Attempts != 1 || done.StatusCode != http.StatusNoContent || done.FinishedAt.IsZero() {
		t.Errorf("succeeded job = %+v", done)
	}

	select {
	case got := <-received:
		if got != "POST yes {\"value\":\"one\"}" {
			t.Errorf("target got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("target did not receive the job")
	}

	data, err := os.ReadFile(filepath.Join(manager.dir, "demo.json"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), `"status":"running"`) {
		t.Errorf("persisted file still has a running job: %s", data)
	}

	info, err := os.Stat(filepath.Join(manager.dir, "demo.json"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("jobs file mode = %o, want 600", info.Mode().Perm())
	}

	// A fresh manager reloads terminal jobs and pending work.
	manager.stop("demo")
	reloaded := newJobManager(manager.dir, s)
	s.jobs = reloaded
	t.Cleanup(reloaded.close)

	if err := reloaded.start("demo", 1); err != nil {
		t.Fatal(err)
	}

	waitJob(t, reloaded.store("demo"), keyed.ID, jobStatusSucceeded)
}

func TestJobQueueRetriesAndDeadLetters(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	address, ok := target.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", target.Listener.Addr())
	}

	_, manager := jobTestSupervisor(t, address.Port)
	store := manager.store("demo")

	job, _, err := store.enqueue(jobEnqueueRequest{Path: "/work", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}

	dead := waitJob(t, store, job.ID, jobStatusDead)
	if dead.Attempts != 2 || dead.LastError == "" || dead.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dead job = %+v", dead)
	}

	if _, err := store.retry(job.ID); err != nil {
		t.Fatal(err)
	}

	fail.Store(false)
	waitJob(t, store, job.ID, jobStatusSucceeded)
}

func TestJobQueueCancelAndAPI(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	address, ok := target.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", target.Listener.Addr())
	}

	s, manager := jobTestSupervisor(t, address.Port)
	store := manager.store("demo")

	job, _, err := store.enqueue(jobEnqueueRequest{Path: "/work", RunAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/jobs/"+job.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	job, _ = store.get(job.ID)
	if job.Status != jobStatusCanceled {
		t.Errorf("canceled job = %+v", job)
	}
}
