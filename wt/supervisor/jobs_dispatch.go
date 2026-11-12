package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// loop dispatches due jobs while the app is ready.
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

// wait sleeps until the duration, context cancellation or a wake signal.
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

// appReady reports whether the app is registered and serving.
func (s *jobStore) appReady() bool {
	a := s.manager.s.lookup(s.app)
	if a == nil {
		return false
	}

	_, ready := a.servingState()
	if !ready && a.idleState() && s.hasDueWork() {
		// A job that is due starts an app that scaled to zero.
		s.manager.s.wakeIdleAppAsync(a)
	}

	return ready
}

// busy reports whether a worker is running a job right now.
func (s *jobStore) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.running > 0
}

// hasDueWork reports whether a pending job is due now.
func (s *jobStore) hasDueWork() bool {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	for index := range s.jobs {
		if s.jobs[index].Status == jobStatusPending && !s.jobs[index].RunAt.After(now) {
			return true
		}
	}

	return false
}

// wakeIdle starts an idle-stopped app so its due jobs can run.
func (s *jobStore) wakeIdle() {
	a := s.manager.s.lookup(s.app)
	if a != nil && a.idleState() {
		s.manager.s.wakeIdleAppAsync(a)
	}
}

// dispatch claims due jobs up to the concurrency limit and starts one goroutine per job.
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

// execute runs one job and records its outcome.
func (s *jobStore) execute(job appJob) {
	defer s.finished(job.ID)

	status, failure := s.runJob(job)
	s.complete(job.ID, status, failure)
}

// runJob sends the job to the app's loopback port and treats a non-2xx response as failure.
func (s *jobStore) runJob(job appJob) (int, error) {
	a := s.manager.s.lookup(s.app)
	if a == nil {
		return 0, errors.New("app is not registered")
	}

	a.mu.Lock()
	port := a.port
	a.mu.Unlock()

	if port == 0 {
		return 0, errors.New("app has no port")
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

// finished frees a worker slot and wakes the dispatcher.
func (s *jobStore) finished(_ string) {
	s.mu.Lock()
	if s.running > 0 {
		s.running--
	}
	s.mu.Unlock()
	s.signal()
}

// complete marks a job succeeded, schedules a retry or marks it dead after the final attempt.
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

// jobBackoff doubles the delay per attempt and caps it at five minutes.
func jobBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	backoff := time.Duration(1<<min(attempt-1, 8)) * time.Second

	return min(backoff, 5*time.Minute)
}
