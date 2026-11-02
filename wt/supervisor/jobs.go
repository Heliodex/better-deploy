// Durable per-app job queue.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	jobStatusPending   = "pending"
	jobStatusRunning   = "running"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
	jobStatusDead      = "dead"
	jobStatusCanceled  = "canceled"

	jobDefaultConcurrency = 1
	jobMaxConcurrency     = 16
	jobDefaultAttempts    = 3
	jobMaxAttempts        = 20
	jobMaxBodyBytes       = 64 << 10
	jobMaxKeyBytes        = 128
	jobMaxJobs            = 1000
	jobLease              = 2 * time.Minute
	jobTimeout            = 2 * time.Minute
	jobPollInterval       = 250 * time.Millisecond
	jobIdleInterval       = 500 * time.Millisecond
)

// appJob is one durable unit of work for an app. Status is terminal for succeeded, dead and canceled.
type appJob struct {
	ID             string            `json:"id"`
	Path           string            `json:"path"`
	Method         string            `json:"method"`
	Body           string            `json:"body,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Status         string            `json:"status"`
	RunAt          time.Time         `json:"runAt"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
	StartedAt      time.Time         `json:"startedAt"`
	FinishedAt     time.Time         `json:"finishedAt"`
	Attempts       int               `json:"attempts"`
	MaxAttempts    int               `json:"maxAttempts"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	LeaseUntil     time.Time         `json:"leaseUntil"`
	LastError      string            `json:"lastError,omitempty"`
	StatusCode     int               `json:"statusCode,omitempty"`
}

type jobFile struct {
	Jobs []appJob `json:"jobs"`
}

type jobsResponse struct {
	Jobs []appJob `json:"jobs"`
}

type jobEnqueueRequest struct {
	Path           string            `json:"path"`
	Method         string            `json:"method"`
	Body           string            `json:"body"`
	Headers        map[string]string `json:"headers"`
	RunAt          time.Time         `json:"runAt"`
	MaxAttempts    int               `json:"maxAttempts"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

// jobManager owns one worker loop per app. Jobs live outside /data so an app's own code cannot change its queue.
type jobManager struct {
	dir string
	s   *supervisor

	mu     sync.Mutex
	stores map[string]*jobStore
}

func newJobManager(dir string, s *supervisor) *jobManager {
	return &jobManager{dir: dir, s: s, stores: map[string]*jobStore{}}
}

func (m *jobManager) path(app string) string {
	return filepath.Join(m.dir, app+".json")
}

func (m *jobManager) start(app string, concurrency int) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if store := m.stores[app]; store != nil {
		store.setConcurrency(concurrency)
		return nil
	}
	store, err := newJobStore(m, app, m.path(app), concurrency)
	if err != nil {
		return err
	}
	m.stores[app] = store
	go store.loop()
	return nil
}

func (m *jobManager) setConcurrency(app string, concurrency int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	store := m.stores[app]
	m.mu.Unlock()
	if store != nil {
		store.setConcurrency(concurrency)
	}
}

func (m *jobManager) stop(app string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	store := m.stores[app]
	delete(m.stores, app)
	m.mu.Unlock()
	if store != nil {
		store.close()
	}
}

func (m *jobManager) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	stores := make([]*jobStore, 0, len(m.stores))
	for _, store := range m.stores {
		stores = append(stores, store)
	}
	m.stores = map[string]*jobStore{}
	m.mu.Unlock()
	for _, store := range stores {
		store.close()
	}
}

func (m *jobManager) store(app string) *jobStore {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stores[app]
}

// jobStore is the durable queue and worker loop for one app.
type jobStore struct {
	manager *jobManager
	app     string
	path    string

	mu          sync.Mutex
	jobs        []appJob
	concurrency int
	running     int
	closed      bool
	wake        chan struct{}
	ctx         context.Context
	cancelFunc  context.CancelFunc
}

func newJobStore(manager *jobManager, app, path string, concurrency int) (*jobStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("jobs dir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("jobs dir: %w", err)
	}
	store := &jobStore{
		manager:     manager,
		app:         app,
		path:        path,
		concurrency: normalizeJobConcurrency(concurrency),
		wake:        make(chan struct{}, 1),
	}
	store.ctx, store.cancelFunc = context.WithCancel(context.Background())
	if err := store.load(); err != nil {
		store.cancelFunc()
		return nil, err
	}
	return store, nil
}

func normalizeJobConcurrency(value int) int {
	if value <= 0 {
		return jobDefaultConcurrency
	}
	if value > jobMaxConcurrency {
		return jobMaxConcurrency
	}
	return value
}

func (s *jobStore) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read jobs: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("jobs file: %w", err)
	}
	var file jobFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("jobs file: %w", err)
	}
	jobs := make([]appJob, 0, len(file.Jobs))
	for _, job := range file.Jobs {
		if job.ID == "" || job.Path == "" || !validJobStatus(job.Status) {
			continue
		}
		// A supervisor restart means no worker owns a running lease. Run it again instead of waiting for the lease.
		if job.Status == jobStatusRunning {
			job.Status = jobStatusPending
			job.LeaseUntil = time.Time{}
			if job.Attempts > 0 {
				job.Attempts--
			}
		}
		if job.MaxAttempts <= 0 {
			job.MaxAttempts = jobDefaultAttempts
		}
		jobs = append(jobs, job)
	}
	if len(jobs) > jobMaxJobs {
		jobs = jobs[len(jobs)-jobMaxJobs:]
	}
	s.jobs = jobs
	return s.saveLocked()
}

func validJobStatus(status string) bool {
	switch status {
	case jobStatusPending, jobStatusRunning, jobStatusSucceeded, jobStatusFailed, jobStatusDead, jobStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *jobStore) saveLocked() error {
	data, err := json.Marshal(jobFile{Jobs: s.jobs})
	if err != nil {
		return err
	}
	if err := writeFileAtomicMode(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	return nil
}

func (s *jobStore) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *jobStore) setConcurrency(value int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.concurrency = normalizeJobConcurrency(value)
	s.mu.Unlock()
	s.signal()
}

func (s *jobStore) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cancelFunc()
}

func (s *jobStore) enqueue(req jobEnqueueRequest) (appJob, bool, error) {
	now := time.Now().UTC()
	path := strings.TrimSpace(req.Path)
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") {
		return appJob{}, false, fmt.Errorf("path must start with / and contain no control characters")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet:
	default:
		return appJob{}, false, fmt.Errorf("method must be GET, POST, PUT, PATCH or DELETE")
	}
	if len(req.Body) > jobMaxBodyBytes {
		return appJob{}, false, fmt.Errorf("body must be at most %d bytes", jobMaxBodyBytes)
	}
	if len(req.IdempotencyKey) > jobMaxKeyBytes {
		return appJob{}, false, fmt.Errorf("idempotencyKey must be at most %d bytes", jobMaxKeyBytes)
	}
	attempts := req.MaxAttempts
	if attempts <= 0 {
		attempts = jobDefaultAttempts
	}
	if attempts > jobMaxAttempts {
		return appJob{}, false, fmt.Errorf("maxAttempts must be between 1 and %d", jobMaxAttempts)
	}
	runAt := req.RunAt.UTC()
	if runAt.IsZero() || runAt.Before(now) {
		runAt = now
	}
	headers := map[string]string{}
	for key, value := range req.Headers {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsAny(value, "\x00\r\n") {
			continue
		}
		headers[key] = value
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey != "" {
		for _, job := range s.jobs {
			if job.IdempotencyKey == req.IdempotencyKey {
				return job, false, nil
			}
		}
	}
	if len(s.jobs) >= jobMaxJobs {
		return appJob{}, false, fmt.Errorf("job queue is full (%d jobs)", jobMaxJobs)
	}
	job := appJob{
		ID:             newJobID(),
		Path:           path,
		Method:         method,
		Body:           req.Body,
		Headers:        headers,
		Status:         jobStatusPending,
		RunAt:          runAt,
		CreatedAt:      now,
		UpdatedAt:      now,
		MaxAttempts:    attempts,
		IdempotencyKey: req.IdempotencyKey,
	}
	s.jobs = append(s.jobs, job)
	if err := s.saveLocked(); err != nil {
		s.jobs = s.jobs[:len(s.jobs)-1]
		return appJob{}, false, err
	}
	s.signal()
	return job, true, nil
}

func newJobID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

func (s *jobStore) list(status string, limit int) []appJob {
	if limit <= 0 || limit > jobMaxJobs {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]appJob, 0, min(limit, len(s.jobs)))
	for index := len(s.jobs) - 1; index >= 0 && len(out) < limit; index-- {
		if status != "" && s.jobs[index].Status != status {
			continue
		}
		out = append(out, s.jobs[index])
	}
	return out
}

func (s *jobStore) get(id string) (appJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.ID == id {
			return job, true
		}
	}
	return appJob{}, false
}

func (s *jobStore) cancel(id string) (appJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.jobs {
		if s.jobs[index].ID != id {
			continue
		}
		if s.jobs[index].Status == jobStatusSucceeded || s.jobs[index].Status == jobStatusDead || s.jobs[index].Status == jobStatusCanceled {
			return s.jobs[index], fmt.Errorf("job already finished")
		}
		s.jobs[index].Status = jobStatusCanceled
		s.jobs[index].FinishedAt = time.Now().UTC()
		s.jobs[index].UpdatedAt = s.jobs[index].FinishedAt
		s.jobs[index].LeaseUntil = time.Time{}
		if err := s.saveLocked(); err != nil {
			return appJob{}, err
		}
		s.manager.s.metrics.observeJob(s.app, "canceled")
		s.signal()
		return s.jobs[index], nil
	}
	return appJob{}, fmt.Errorf("job not found")
}

func (s *jobStore) retry(id string) (appJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.jobs {
		if s.jobs[index].ID != id {
			continue
		}
		switch s.jobs[index].Status {
		case jobStatusDead, jobStatusFailed, jobStatusCanceled:
		default:
			return s.jobs[index], fmt.Errorf("only dead, failed or canceled jobs can be retried")
		}
		s.jobs[index].Status = jobStatusPending
		s.jobs[index].Attempts = 0
		s.jobs[index].RunAt = time.Now().UTC()
		s.jobs[index].LeaseUntil = time.Time{}
		s.jobs[index].StartedAt = time.Time{}
		s.jobs[index].FinishedAt = time.Time{}
		s.jobs[index].LastError = ""
		s.jobs[index].StatusCode = 0
		s.jobs[index].UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			return appJob{}, err
		}
		s.signal()
		return s.jobs[index], nil
	}
	return appJob{}, fmt.Errorf("job not found")
}

func (s *jobStore) loop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		if !s.appReady() {
			if !s.wait(jobIdleInterval) {
				return
			}
			continue
		}
		if s.dispatch() {
			continue
		}
		if !s.wait(jobPollInterval) {
			return
		}
	}
}

func (s *jobStore) wait(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *jobStore) appReady() bool {
	a := s.manager.s.lookup(s.app)
	if a == nil {
		return false
	}
	_, ready := a.servingState()
	return ready
}

// dispatch claims due jobs up to the concurrency limit and starts one goroutine per job.
func (s *jobStore) dispatch() bool {
	now := time.Now().UTC()
	s.mu.Lock()
	if s.running >= s.concurrency {
		s.mu.Unlock()
		return false
	}
	var claimed []appJob
	for index := range s.jobs {
		if s.running+len(claimed) >= s.concurrency {
			break
		}
		if s.jobs[index].Status != jobStatusPending || s.jobs[index].RunAt.After(now) {
			continue
		}
		s.jobs[index].Status = jobStatusRunning
		s.jobs[index].Attempts++
		s.jobs[index].StartedAt = now
		s.jobs[index].UpdatedAt = now
		s.jobs[index].LeaseUntil = now.Add(jobLease)
		s.jobs[index].LastError = ""
		claimed = append(claimed, s.jobs[index])
	}
	if len(claimed) == 0 {
		s.mu.Unlock()
		return false
	}
	s.running += len(claimed)
	if err := s.saveLocked(); err != nil {
		log.Printf("jobs %s: save: %v", s.app, err)
	}
	s.mu.Unlock()
	for _, job := range claimed {
		go s.execute(job)
	}
	return true
}

func (s *jobStore) execute(job appJob) {
	defer s.finished(job.ID)
	status, failure := s.runJob(job)
	s.complete(job.ID, status, failure)
}

func (s *jobStore) runJob(job appJob) (int, error) {
	a := s.manager.s.lookup(s.app)
	if a == nil {
		return 0, fmt.Errorf("app is not registered")
	}
	a.mu.Lock()
	port := a.port
	a.mu.Unlock()
	if port == 0 {
		return 0, fmt.Errorf("app has no port")
	}
	body := strings.NewReader(job.Body)
	request, err := http.NewRequestWithContext(s.ctx, job.Method, fmt.Sprintf("http://127.0.0.1:%d%s", port, job.Path), body)
	if err != nil {
		return 0, err
	}
	if job.Body != "" && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range job.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set(requestIDHeader, "job-"+job.ID)
	client := &http.Client{Timeout: jobTimeout}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response.StatusCode, fmt.Errorf("job endpoint returned %s", response.Status)
	}
	return response.StatusCode, nil
}

func (s *jobStore) finished(_ string) {
	s.mu.Lock()
	if s.running > 0 {
		s.running--
	}
	s.mu.Unlock()
	s.signal()
}

func (s *jobStore) complete(id string, status int, failure error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.jobs {
		if s.jobs[index].ID != id {
			continue
		}
		job := &s.jobs[index]
		if job.Status != jobStatusRunning {
			return
		}
		job.UpdatedAt = now
		job.StatusCode = status
		job.LeaseUntil = time.Time{}
		if failure == nil {
			job.Status = jobStatusSucceeded
			job.FinishedAt = now
			job.LastError = ""
			s.manager.s.metrics.observeJob(s.app, "succeeded")
		} else {
			job.LastError = failure.Error()
			if job.Attempts >= job.MaxAttempts {
				job.Status = jobStatusDead
				job.FinishedAt = now
				s.manager.s.metrics.observeJob(s.app, "dead")
			} else {
				job.Status = jobStatusPending
				job.RunAt = now.Add(jobBackoff(job.Attempts))
				s.manager.s.metrics.observeJob(s.app, "retry")
			}
		}
		if err := s.saveLocked(); err != nil {
			log.Printf("jobs %s: save: %v", s.app, err)
		}
		return
	}
}

func jobBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := time.Duration(1<<min(attempt-1, 8)) * time.Second
	return min(backoff, 5*time.Minute)
}

// handleAppJobs serves the queue resource for one app.
func (s *supervisor) handleAppJobs(w http.ResponseWriter, r *http.Request, name, action string) {
	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}
	store := s.jobs.store(name)
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	if action == "jobs" {
		switch r.Method {
		case http.MethodGet:
			status := r.URL.Query().Get("status")
			if status != "" && !validJobStatus(status) {
				writeError(w, http.StatusBadRequest, "unknown job status")
				return
			}
			writeJSON(w, http.StatusOK, jobsResponse{Jobs: store.list(status, 100)})
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, jobMaxBodyBytes+maxConfigRequestBytes)
			var req jobEnqueueRequest
			if err := json.UnmarshalRead(r.Body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			job, created, err := store.enqueue(req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			writeJSON(w, status, job)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	rest := strings.TrimPrefix(action, "jobs/")
	id, verb, _ := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case verb == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		job, ok := store.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, job)
	case verb == "cancel" && r.Method == http.MethodPost:
		job, err := store.cancel(id)
		if err != nil {
			status := http.StatusConflict
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	case verb == "retry" && r.Method == http.MethodPost:
		job, err := store.retry(id)
		if err != nil {
			status := http.StatusConflict
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	default:
		http.Error(w, "unknown job endpoint", http.StatusNotFound)
	}
}
