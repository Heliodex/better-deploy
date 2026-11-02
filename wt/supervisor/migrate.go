// The explicit migration phase: run the app's reserved migrate endpoint before probing health.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// migratePath is the reserved SDK endpoint the supervisor calls before probing health.
// It must match app.MigratePath in wt/packages/app.
const migratePath = "/_wt/migrate"

var errNoMigrateEndpoint = errors.New("app has no migrate endpoint")

// migrateApp runs the SDK's explicit migration endpoint before readiness.
// A migration can take much longer than startup_timeout, so it gets the storage migration timeout instead of the startup deadline.
func (s *supervisor) migrateApp(a *app, port int, done <-chan struct{}) error {
	timeout := s.cfg.storageMigrationTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	a.mu.Lock()
	startup := a.cfg.startupLimit()
	a.mu.Unlock()

	readyBy := time.Now().Add(startup)
	deadline := time.Now().Add(timeout + 10*time.Second)
	client := &http.Client{
		Timeout: timeout + 5*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", port, migratePath)

	var last string

	wait := 50 * time.Millisecond

	for {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, nil)
		if err != nil {
			return err
		}

		response, err := client.Do(request)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))

			_ = response.Body.Close()
			switch {
			case response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK:
				return nil
			case response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusInternalServerError:
				// 3xx and 4xx mean the app does not implement the reserved endpoint, for example a router redirect or an auth layer.
				return errNoMigrateEndpoint
			case response.StatusCode == http.StatusServiceUnavailable:
				last = response.Status + ": " + trimMessage(body)
				if time.Now().After(readyBy) {
					return fmt.Errorf("storage unavailable: %s", last)
				}
			default:
				return fmt.Errorf("%s: %s", response.Status, trimMessage(body))
			}
		} else {
			last = err.Error()
			if time.Now().After(readyBy) {
				return fmt.Errorf("migrate endpoint: %w", err)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("migration timed out: %s", last)
		}

		select {
		case <-done:
			return fmt.Errorf("app exited before migration: %s", last)
		case <-time.After(wait):
		}

		wait = min(wait*2, time.Second)
	}
}
