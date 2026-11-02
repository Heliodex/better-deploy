package app

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const jobsPath = "/v1/jobs"

// Job status values returned by the supervisor.
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobDead      = "dead"
	JobCanceled  = "canceled"
)

// Job is one durable unit of work as returned by the supervisor.
type Job struct {
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

// JobRequest is the body for a new job. Body is already-encoded JSON or text; it is sent verbatim to the app.
type JobRequest struct {
	Path           string            `json:"path"`
	Method         string            `json:"method,omitempty"`
	Body           string            `json:"body,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	RunAt          time.Time         `json:"runAt,omitempty,omitzero"`
	MaxAttempts    int               `json:"maxAttempts,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
}

type jobsResponse struct {
	Jobs []Job `json:"jobs"`
}

// jobEndpoint reads the storage loopback URL and token that guard an app's job queue.
func jobEndpoint() (string, string, error) {
	base := strings.TrimSuffix(os.Getenv(storageURLEnv), "/")
	token := os.Getenv(storageTokenEnv)
	if base == "" || token == "" {
		return "", "", fmt.Errorf("%s and %s are not set", storageURLEnv, storageTokenEnv)
	}
	return base, token, nil
}

// jobDo performs one authenticated job request and decodes its JSON response.
func jobDo(ctx context.Context, client *http.Client, method, path string, body, out any) (int, error) {
	base, token, err := jobEndpoint()
	if err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+jobsPath+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, fmt.Errorf("job request %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode job response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// EnqueueJob adds a job to the current app's durable queue. created is false when an idempotency key matched an existing job.
func EnqueueJob(ctx context.Context, client *http.Client, request JobRequest) (Job, bool, error) {
	var job Job
	status, err := jobDo(ctx, client, http.MethodPost, "", request, &job)
	return job, status == http.StatusCreated, err
}

// ListJobs returns the current app's newest jobs, optionally filtered by status.
func ListJobs(ctx context.Context, client *http.Client, status string) ([]Job, error) {
	path := ""
	if status != "" {
		path = "?status=" + url.QueryEscape(status)
	}
	var result jobsResponse
	if _, err := jobDo(ctx, client, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result.Jobs, nil
}

// GetJob returns one job by ID.
func GetJob(ctx context.Context, client *http.Client, id string) (Job, error) {
	var job Job
	_, err := jobDo(ctx, client, http.MethodGet, "/"+url.PathEscape(id), nil, &job)
	return job, err
}

// CancelJob cancels a pending or running job.
func CancelJob(ctx context.Context, client *http.Client, id string) (Job, error) {
	var job Job
	_, err := jobDo(ctx, client, http.MethodPost, "/"+url.PathEscape(id)+"/cancel", nil, &job)
	return job, err
}

// RetryJob requeues a dead, failed or canceled job.
func RetryJob(ctx context.Context, client *http.Client, id string) (Job, error) {
	var job Job
	_, err := jobDo(ctx, client, http.MethodPost, "/"+url.PathEscape(id)+"/retry", nil, &job)
	return job, err
}
