package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/v2/app"
	wasihttp "github.com/heliodex/better-deploy/wt/packages/v2/wasihttp"
)

// beatLevels rotates so the panel demonstrates every severity. It is derived from the clock rather than a counter because each request may get a fresh component instance.
var beatLevels = []app.Level{
	app.LevelTrace,
	app.LevelDebug,
	app.LevelInfo,
	app.LevelWarn,
	app.LevelError,
	app.LevelFatal,
}

// handleTick is driven by the schedule in app.json, not by a client. Components under `wasmtime serve` only run while handling a request, so the supervisor fires this endpoint every ten seconds and the panel stamps each log line.
// It also enqueues one durable job per slot, which shows a payload that survives a restart and is retried if the worker call fails.
func handleTick(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	slot := now.Unix() / 10
	app.Logf(beatLevels[int(slot%int64(len(beatLevels)))], "ticker beat")

	job, created, err := app.EnqueueJob(r.Context(), wasihttp.NewClient(), app.JobRequest{
		Path:           "/jobs/beat",
		Method:         http.MethodPost,
		Body:           fmt.Sprintf(`{"slot":%d,"at":%q}`, slot, now.Format(time.RFC3339Nano)),
		MaxAttempts:    3,
		IdempotencyKey: fmt.Sprintf("ticker-%d", slot),
	})
	if err != nil {
		app.Errorf("ticker job: %v", err)
	} else if created {
		app.Infof("ticker queued %s", job.ID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleJobBeat is the durable job target. The worker calls it through the app's loopback port and retries failures.
func handleJobBeat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<10))
	app.Infof("ticker job beat %s", strings.TrimSpace(string(body)))
	w.WriteHeader(http.StatusNoContent)
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ticker logs a beat every 10s from the supervisor's schedule,")
	fmt.Fprintln(w, "cycling TRACE, DEBUG, INFO, WARN, ERROR and FATAL.")
	fmt.Fprintln(w, "each slot also enqueues a durable /jobs/beat job with a payload.")
}

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tick", handleTick)
	mux.HandleFunc("POST /jobs/beat", handleJobBeat)
	mux.HandleFunc("/", handleIndex)
	wasihttp.Handle(mux.ServeHTTP)
}

// main is required to build, but the component's entry point is the exported wasi:http handler.
func main() {}
