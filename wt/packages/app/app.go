// Package app holds the small HTTP and storage helpers shared by the example apps.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// TimestampFormat is the fixed-width UTC layout used for stored and served times. Text ordering then matches chronological ordering.
const TimestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// NewID returns a short random identifier for a stored row.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// WriteJSON writes value as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := json.Marshal(value)
	_, _ = w.Write(append(data, '\n'))
}

// WriteError writes {"error": message} with the given status.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// HealthPath is the reserved readiness path the supervisor probes.
const HealthPath = "/healthz"

// MigratePath is the reserved endpoint the supervisor calls before it probes health.
// It runs the same migration as the first request, so a slow schema change does not race the supervisor's startup timeout or probe timeout.
const MigratePath = "/_wt/migrate"

// ServeStorage runs next behind the storage service. It answers 503 while the storage client is unavailable, and 500 until the migration succeeds; a failed migration is retried on the next request, a successful one runs only once.
func ServeStorage(storeErr error, migrate func(context.Context) error, next http.Handler) http.Handler {
	var mu sync.Mutex
	ready := false
	ensureReady := func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if ready {
			return nil
		}
		// A probe or migrate request may disconnect while the schema change is running.
		// Keep the context values but not its cancellation, and let the storage service enforce its own migration timeout.
		if err := migrate(context.WithoutCancel(ctx)); err != nil {
			return err
		}
		ready = true
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if storeErr != nil {
			http.Error(w, "storage unavailable: "+storeErr.Error(), http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == MigratePath {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := ensureReady(r.Context()); err != nil {
				http.Error(w, "storage schema: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := ensureReady(r.Context()); err != nil {
			http.Error(w, "storage schema: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// The supervisor readiness probe answers after migrations have run, so it never reports ready against an unmigrated store.
		if r.URL.Path == HealthPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
