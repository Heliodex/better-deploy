package main

import (
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// jobRecord is the subset of the supervisor's job resource the CLI prints.
type jobRecord struct {
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

type jobsResponse struct {
	Jobs []jobRecord `json:"jobs"`
}

// jobTime renders a job timestamp, or a placeholder for the zero time.
func jobTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}

	return value.UTC().Format(time.RFC3339)
}

// printJobs prints the newest jobs first, or the raw response with --json.
func printJobs(cfg clientOptions, body []byte) error {
	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}

	var result jobsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	if len(result.Jobs) == 0 {
		fmt.Println("no jobs")
		return nil
	}

	fmt.Printf("%-26s %-10s %-7s %-8s %-20s %s\n", "ID", "STATUS", "METHOD", "ATTEMPTS", "RUN AT", "PATH")

	for _, job := range result.Jobs {
		fmt.Printf("%-26s %-10s %-7s %-8s %-20s %s\n", job.ID, job.Status, job.Method, fmt.Sprintf("%d/%d", job.Attempts, job.MaxAttempts), jobTime(job.RunAt), job.Path)
	}

	return nil
}

// printJob prints one job's fields.
func printJob(job jobRecord) {
	fmt.Printf("ID: %s\n", job.ID)
	fmt.Printf("Status: %s\n", job.Status)
	fmt.Printf("Path: %s %s\n", job.Method, job.Path)
	fmt.Printf("Attempts: %d/%d\n", job.Attempts, job.MaxAttempts)
	fmt.Printf("Run at: %s\n", jobTime(job.RunAt))
	fmt.Printf("Created: %s\n", jobTime(job.CreatedAt))
	fmt.Printf("Started: %s\n", jobTime(job.StartedAt))
	fmt.Printf("Finished: %s\n", jobTime(job.FinishedAt))

	if job.StatusCode != 0 {
		fmt.Printf("Status code: %d\n", job.StatusCode)
	}

	if job.Body != "" {
		fmt.Printf("Body: %s\n", job.Body)
	}

	if len(job.Headers) > 0 {
		keys := make([]string, 0, len(job.Headers))
		for key := range job.Headers {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		for _, key := range keys {
			fmt.Printf("Header: %s: %s\n", key, job.Headers[key])
		}
	}

	if job.LastError != "" {
		fmt.Printf("Last error: %s\n", job.LastError)
	}

	if job.IdempotencyKey != "" {
		fmt.Printf("Idempotency key: %s\n", job.IdempotencyKey)
	}
}

// jobsList requests one page of jobs, optionally filtered by status.
func jobsList(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("jobs list", flag.ContinueOnError)
	status := flags.String("status", "", "pending, running, succeeded, failed, dead or canceled")

	jsonOut := flags.Bool("json", false, "print raw JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	if *status != "" && !validJobStatus(*status) {
		return fmt.Errorf("unknown job status %q", *status)
	}

	if *jsonOut {
		cfg.jsonOut = true
	}

	path := "/apps/" + name + "/jobs"
	if *status != "" {
		path += "?status=" + url.QueryEscape(*status)
	}

	statusCode, _, body, err := apiDo(cfg, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(statusCode, body); err != nil {
		return err
	}

	return printJobs(cfg, body)
}

// jobsShow requests one job.
func jobsShow(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("jobs show", flag.ContinueOnError)

	jsonOut := flags.Bool("json", false, "print raw JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 1 {
		return errors.New("usage: wt jobs <app> show <id>")
	}

	if *jsonOut {
		cfg.jsonOut = true
	}

	id := flags.Arg(0)

	statusCode, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/jobs/"+url.PathEscape(id), nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(statusCode, body); err != nil {
		return err
	}

	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}

	var job jobRecord
	if err := json.Unmarshal(body, &job); err != nil {
		return err
	}

	printJob(job)

	return nil
}

// jobsEnqueue creates a durable job from flags.
func jobsEnqueue(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("jobs enqueue", flag.ContinueOnError)
	path := flags.String("path", "", "target path, for example /jobs/email")
	method := flags.String("method", http.MethodPost, "HTTP method")
	body := flags.String("body", "", "request body")
	file := flags.String("file", "", "read the request body from a file")
	runAt := flags.String("run-at", "", "RFC3339 time to run the job")
	maxAttempts := flags.Int("max-attempts", 0, "maximum attempts (1-20)")
	idempotencyKey := flags.String("idempotency-key", "", "return the existing job for this key")
	jsonOut := flags.Bool("json", false, "print raw JSON")
	headers := map[string]string{}

	flags.Func("header", "request header in Key: Value form (repeatable)", func(value string) error {
		key, value, ok := strings.Cut(value, ":")
		if !ok {
			key, value, ok = strings.Cut(value, "=")
		}

		if !ok || strings.TrimSpace(key) == "" {
			return errors.New("header must be Key: Value")
		}

		headers[strings.TrimSpace(key)] = strings.TrimSpace(value)

		return nil
	})

	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	if *path == "" || !strings.HasPrefix(*path, "/") {
		return errors.New("--path must start with /")
	}

	if *body != "" && *file != "" {
		return errors.New("use --body or --file, not both")
	}

	if *maxAttempts < 0 {
		return errors.New("--max-attempts must be 0 or more")
	}

	if *jsonOut {
		cfg.jsonOut = true
	}

	payload := map[string]any{"path": *path, "method": strings.ToUpper(*method)}
	text := *body

	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}

		text = string(data)
	}

	if text != "" {
		payload["body"] = text
	}

	if len(headers) > 0 {
		payload["headers"] = headers
	}

	if *runAt != "" {
		parsed, err := time.Parse(time.RFC3339, *runAt)
		if err != nil {
			return fmt.Errorf("--run-at: %w", err)
		}

		payload["runAt"] = parsed.UTC()
	}

	if *maxAttempts > 0 {
		payload["maxAttempts"] = *maxAttempts
	}

	if *idempotencyKey != "" {
		payload["idempotencyKey"] = *idempotencyKey
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	status, _, response, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/jobs", strings.NewReader(string(data)), "application/json")
	if err != nil {
		return err
	}

	if err := checkAPI(status, response); err != nil {
		return err
	}

	if cfg.jsonOut {
		printRawOrJSON(cfg, response)
		return nil
	}

	var job jobRecord
	if err := json.Unmarshal(response, &job); err != nil {
		return err
	}

	verb := "enqueued"
	if status == http.StatusOK {
		verb = "existing"
	}

	fmt.Printf("%s %s %s %s\n", verb, job.ID, job.Status, job.Path)

	return nil
}

// jobsAction cancels or retries one job.
func jobsAction(cfg clientOptions, name, action string, args []string) error {
	flags := flag.NewFlagSet("jobs "+action, flag.ContinueOnError)

	jsonOut := flags.Bool("json", false, "print raw JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 1 {
		return fmt.Errorf("usage: wt jobs <app> %s <id>", action)
	}

	if *jsonOut {
		cfg.jsonOut = true
	}

	id := flags.Arg(0)

	status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/jobs/"+url.PathEscape(id)+"/"+action, nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(status, body); err != nil {
		return err
	}

	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}

	var job jobRecord
	if err := json.Unmarshal(body, &job); err != nil {
		return err
	}

	fmt.Printf("%s %s %s\n", action, job.ID, job.Status)

	return nil
}

// validJobStatus reports whether status is one the supervisor accepts.
func validJobStatus(status string) bool {
	switch status {
	case "pending", "running", "succeeded", "failed", "dead", "canceled":
		return true
	}

	return false
}

// runJobs dispatches wt jobs <app> <action>.
func runJobs(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) < 2 {
		return errors.New("usage: wt jobs <app> <list|show|enqueue|cancel|retry>")
	}

	name, action := rest[0], rest[1]
	if !validName(name) {
		return fmt.Errorf("invalid app name %q", name)
	}

	switch action {
	case "list":
		return jobsList(cfg, name, rest[2:])
	case "show":
		return jobsShow(cfg, name, rest[2:])
	case "enqueue":
		return jobsEnqueue(cfg, name, rest[2:])
	case "cancel", "retry":
		return jobsAction(cfg, name, action, rest[2:])
	default:
		return fmt.Errorf("unknown jobs action %q", action)
	}
}
