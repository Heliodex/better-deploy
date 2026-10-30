package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// runSchedule calls one endpoint on its interval. Calls are sequential, so a slow endpoint delays the next tick instead of letting requests pile up.
func (s *supervisor) runSchedule(ctx context.Context, a *app, entry scheduleConfig) {
	every, err := parseScheduleInterval(entry)
	if err != nil {
		log.Printf("app %s: schedule %s: %v", a.name, entry.Path, err)
		return
	}
	method := entry.Method
	if method == "" {
		method = defaultScheduleMethod
	}
	// A hung endpoint must not stack ticks, but work that legitimately outlasts the interval should still be allowed to finish.
	client := &http.Client{Timeout: max(every, 5*time.Second)}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", a.port, entry.Path)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.stopping.Load() {
			return
		}
		request, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			log.Printf("app %s: schedule %s: %v", a.name, entry.Path, err)
			continue
		}
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("app %s: schedule %s: %v", a.name, entry.Path, err)
			continue
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode >= http.StatusBadRequest {
			log.Printf("app %s: schedule %s: %s", a.name, entry.Path, response.Status)
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
	a.mu.Unlock()
	for _, entry := range entries {
		go s.runSchedule(ctx, a, entry)
	}
}
