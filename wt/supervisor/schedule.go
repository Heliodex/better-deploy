package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// scheduleRun is the outcome of an entry's latest call, reported in the app's info.
type scheduleRun struct {
	Name     string    `json:"name"`
	Every    string    `json:"every"`
	LastRun  time.Time `json:"lastRun,omitzero"`
	Duration string    `json:"duration,omitempty"`
	Status   int       `json:"status,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Failures counts consecutive failed calls; a success resets it.
	Failures int       `json:"failures,omitempty"`
	NextRun  time.Time `json:"nextRun,omitzero"`
}

// scheduleLabels names each entry: its name, or its method and path. Repeated default labels get a #n suffix, so every entry keeps its own status.
func scheduleLabels(entries []scheduleConfig) []string {
	labels := make([]string, len(entries))
	seen := map[string]int{}

	for i, entry := range entries {
		label := entry.Name
		if label == "" {
			method := entry.Method
			if method == "" {
				method = defaultScheduleMethod
			}

			label = method + " " + entry.Path
		}

		seen[label]++
		if seen[label] > 1 {
			label += "#" + strconv.Itoa(seen[label])
		}

		labels[i] = label
	}

	return labels
}

// scheduleRunsLocked lists the entries' latest calls in configuration order. The caller holds a.mu.
func (a *app) scheduleRunsLocked() []scheduleRun {
	if len(a.cfg.Schedule) == 0 {
		return nil
	}

	labels := scheduleLabels(a.cfg.Schedule)
	runs := make([]scheduleRun, len(labels))

	for i, label := range labels {
		run, ok := a.scheduleRuns[label]
		if !ok {
			run = scheduleRun{Name: label}
		}

		run.Every = a.cfg.Schedule[i].Every
		runs[i] = run
	}

	return runs
}

// recordScheduleRun stores an entry's latest call, carrying the failure streak forward.
func (a *app) recordScheduleRun(run scheduleRun, failed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.scheduleRuns == nil {
		a.scheduleRuns = map[string]scheduleRun{}
	}

	if failed {
		run.Failures = a.scheduleRuns[run.Name].Failures + 1
	}

	a.scheduleRuns[run.Name] = run
}

// callSchedule makes one scheduled call and describes its outcome. Only a cancelled context returns ok false.
func callSchedule(ctx context.Context, client *http.Client, method, target, label string) (run scheduleRun, failed, ok bool) {
	run = scheduleRun{Name: label, LastRun: time.Now().UTC()}

	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		run.Error = err.Error()
		return run, true, true
	}

	request.Header.Set(scheduleHeader, label)

	response, err := client.Do(request)
	run.Duration = time.Since(run.LastRun).Round(time.Millisecond).String()

	if err != nil {
		if ctx.Err() != nil {
			return run, false, false
		}

		run.Error = err.Error()

		return run, true, true
	}

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	run.Status = response.StatusCode
	if response.StatusCode >= http.StatusBadRequest {
		run.Error = response.Status
		return run, true, true
	}

	return run, false, true
}

// runSchedule calls one endpoint on its interval. Calls are sequential, so a slow endpoint delays the next tick instead of letting requests pile up.
func (s *supervisor) runSchedule(ctx context.Context, a *app, entry scheduleConfig, label string) {
	every, err := parseScheduleInterval(entry)
	if err != nil {
		log.Printf("app %s: schedule %s: %v", a.name, label, err)
		return
	}

	// A hung endpoint must not stack ticks, but work that legitimately outlasts the interval should still be allowed to finish.
	timeout, err := parseScheduleTimeout(entry, every)
	if err != nil {
		log.Printf("app %s: schedule %s: %v", a.name, label, err)
		return
	}

	method := entry.Method
	if method == "" {
		method = defaultScheduleMethod
	}

	client := &http.Client{Timeout: timeout}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", a.port, entry.Path)

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	a.recordScheduleRun(scheduleRun{Name: label, NextRun: time.Now().Add(every).UTC()}, false)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if s.stopping.Load() {
			return
		}

		run, failed, ok := callSchedule(ctx, client, method, target, label)
		if !ok {
			return
		}

		run.NextRun = time.Now().Add(every).UTC()
		a.recordScheduleRun(run, failed)

		if failed {
			log.Printf("app %s: schedule %s: %s", a.name, label, run.Error)
		}
	}
}

// stopSchedule cancels the app's scheduled calls, if any.
func (s *supervisor) stopSchedule(a *app) {
	a.mu.Lock()
	cancel := a.scheduleCancel
	a.scheduleCancel = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// startSchedule launches one goroutine per scheduled endpoint. It runs once the app is serving, and is cancelled when the app stops or is replaced.
func (s *supervisor) startSchedule(a *app) {
	a.mu.Lock()
	entries := append([]scheduleConfig(nil), a.cfg.Schedule...)
	a.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.scheduleCancel = cancel
	a.scheduleRuns = map[string]scheduleRun{}
	a.mu.Unlock()

	for i, label := range scheduleLabels(entries) {
		go s.runSchedule(ctx, a, entries[i], label)
	}
}
