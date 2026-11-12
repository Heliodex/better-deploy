package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func newJobID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	return hex.EncodeToString(value[:])
}

// jobDraft is a validated enqueue request, ready to become a stored job.
type jobDraft struct {
	path, method string
	runAt        time.Time
	attempts     int
	headers      map[string]string
}

// validateEnqueue checks an enqueue request and normalises its path, method, run time, attempt limit and headers. It does not touch the queue.
func validateEnqueue(req jobEnqueueRequest, now time.Time) (jobDraft, error) {
	path := strings.TrimSpace(req.Path)
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") {
		return jobDraft{}, errors.New("path must start with / and contain no control characters")
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}

	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet:
	default:
		return jobDraft{}, errors.New("method must be GET, POST, PUT, PATCH or DELETE")
	}

	if len(req.Body) > jobMaxBodyBytes {
		return jobDraft{}, fmt.Errorf("body must be at most %d bytes", jobMaxBodyBytes)
	}

	if len(req.IdempotencyKey) > jobMaxKeyBytes {
		return jobDraft{}, fmt.Errorf("idempotencyKey must be at most %d bytes", jobMaxKeyBytes)
	}

	attempts := req.MaxAttempts
	if attempts <= 0 {
		attempts = jobDefaultAttempts
	}

	if attempts > jobMaxAttempts {
		return jobDraft{}, fmt.Errorf("maxAttempts must be between 1 and %d", jobMaxAttempts)
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

	return jobDraft{path: path, method: method, runAt: runAt, attempts: attempts, headers: headers}, nil
}

// jobByIdempotencyKey returns the stored job with this key. An empty key never matches. The caller must hold s.mu.
func (s *jobStore) jobByIdempotencyKey(key string) (appJob, bool) {
	if key == "" {
		return appJob{}, false
	}

	for _, job := range s.jobs {
		if job.IdempotencyKey == key {
			return job, true
		}
	}

	return appJob{}, false
}

// makeRoomLocked prunes finished jobs and fails when the queue is still full. Pruning is saved even when the queue stays full.
func (s *jobStore) makeRoomLocked() error {
	before := len(s.jobs)
	s.jobs = pruneTerminalJobs(s.jobs)
	pruned := len(s.jobs) != before

	if len(s.jobs) < jobMaxJobs {
		return nil
	}

	if pruned {
		_ = s.saveLocked()
	}

	return fmt.Errorf("job queue is full (%d jobs)", jobMaxJobs)
}

// enqueue validates and persists a new job, deduplicating by idempotency key.
func (s *jobStore) enqueue(req jobEnqueueRequest) (appJob, bool, error) {
	now := time.Now().UTC()

	draft, err := validateEnqueue(req, now)
	if err != nil {
		return appJob{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.jobByIdempotencyKey(req.IdempotencyKey); ok {
		return existing, false, nil
	}

	if err := s.makeRoomLocked(); err != nil {
		return appJob{}, false, err
	}

	job := appJob{
		ID:             newJobID(),
		Path:           draft.path,
		Method:         draft.method,
		Body:           req.Body,
		Headers:        draft.headers,
		Status:         jobStatusPending,
		RunAt:          draft.runAt,
		CreatedAt:      now,
		UpdatedAt:      now,
		MaxAttempts:    draft.attempts,
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

// list returns the newest jobs first, optionally filtered by status.
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

// cancel marks an unfinished job canceled and saves the queue.
func (s *jobStore) cancel(id string) (appJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for index := range s.jobs {
		if s.jobs[index].ID != id {
			continue
		}

		if s.jobs[index].Status == jobStatusSucceeded || s.jobs[index].Status == jobStatusDead || s.jobs[index].Status == jobStatusCanceled {
			return s.jobs[index], errors.New("job already finished")
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

	return appJob{}, errors.New("job not found")
}

// retry resets a dead, failed or canceled job to pending and clears its attempts.
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
			return s.jobs[index], errors.New("only dead, failed or canceled jobs can be retried")
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

	return appJob{}, errors.New("job not found")
}
