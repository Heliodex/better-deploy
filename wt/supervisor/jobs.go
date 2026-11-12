// Durable per-app job queue.

package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	jobTerminalRetention  = 500
	jobLease              = 2 * time.Minute
	jobTimeout            = 2 * time.Minute
	jobPollInterval       = 250 * time.Millisecond
	jobIdleInterval       = 500 * time.Millisecond
)

// terminalJob reports whether a job has finished and can be pruned from the retained history.
func terminalJob(status string) bool {
	switch status {
	case jobStatusSucceeded, jobStatusFailed, jobStatusDead, jobStatusCanceled:
		return true
	default:
		return false
	}
}

// pruneTerminalJobs keeps every active job and only the newest terminal jobs, so a steady producer cannot fill the queue with history.
func pruneTerminalJobs(jobs []appJob) []appJob {
	out := make([]appJob, 0, len(jobs))
	remaining := jobTerminalRetention

	for _, job := range slices.Backward(jobs) {
		if terminalJob(job.Status) {
			if remaining == 0 {
				continue
			}

			remaining--
		}

		out = append(out, job)
	}

	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}

	return out
}

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

// start creates or updates an app's job store and starts its worker loop.
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

// setConcurrency updates a running app's job limit.
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

// stop removes and closes one app's job store.
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

// close shuts down every app's job store.
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

// store returns the job store for an app, or nil when it has none.
func (m *jobManager) store(app string) *jobStore {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stores[app]
}

// busy reports whether the app is running a job right now, so the idle monitor does not stop its process mid-job.
func (m *jobManager) busy(app string) bool {
	store := m.store(app)
	if store == nil {
		return false
	}

	return store.busy()
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
		concurrency: normaliseJobConcurrency(concurrency),
		wake:        make(chan struct{}, 1),
	}

	store.ctx, store.cancelFunc = context.WithCancel(context.Background())
	if err := store.load(); err != nil {
		store.cancelFunc()
		return nil, err
	}

	return store, nil
}

// normaliseJobConcurrency clamps the requested worker limit to the supported range.
func normaliseJobConcurrency(value int) int {
	if value <= 0 {
		return jobDefaultConcurrency
	}

	if value > jobMaxConcurrency {
		return jobMaxConcurrency
	}

	return value
}

// load reads the queue, dropping invalid entries and requeueing jobs left running by a restart.
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

	s.jobs = pruneTerminalJobs(jobs)

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

// saveLocked writes the queue atomically; the caller holds the lock.
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

// signal wakes the worker loop without blocking.
func (s *jobStore) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// setConcurrency clamps and applies a new worker limit.
func (s *jobStore) setConcurrency(value int) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.concurrency = normaliseJobConcurrency(value)
	s.mu.Unlock()
	s.signal()
}

// close cancels the worker loop.
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

// writeJobActionError maps a job action failure to its HTTP status.
