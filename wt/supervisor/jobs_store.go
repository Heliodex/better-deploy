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

// enqueue validates and persists a new job, deduplicating by idempotency key.
func (s *jobStore) enqueue(req jobEnqueueRequest) (appJob, bool, error) {
	now := time.Now().UTC()

	path := strings.TrimSpace(req.Path)
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") {
		return appJob{}, false, errors.New("path must start with / and contain no control characters")
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}

	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet:
	default:
		return appJob{}, false, errors.New("method must be GET, POST, PUT, PATCH or DELETE")
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

	before := len(s.jobs)
	s.jobs = pruneTerminalJobs(s.jobs)
	pruned := len(s.jobs) != before

	if len(s.jobs) >= jobMaxJobs {
		if pruned {
			_ = s.saveLocked()
		}

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
