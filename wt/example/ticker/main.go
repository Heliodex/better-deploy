package main

import (
	"fmt"
	"net/http"
	"time"

	"wt.packages/app"
	wasihttp "wt.packages/wasihttp"
)

// beatLevels rotates so the panel demonstrates every severity. It is derived
// from the clock rather than a counter because each request may get a fresh
// component instance.
var beatLevels = []app.Level{
	app.LevelTrace,
	app.LevelDebug,
	app.LevelInfo,
	app.LevelWarn,
	app.LevelError,
	app.LevelFatal,
}

// handleTick is driven by the schedule in app.json, not by a client. Components
// under `wasmtime serve` only run while handling a request, so the supervisor
// fires this endpoint every ten seconds and the panel stamps each log line.
func handleTick(w http.ResponseWriter, r *http.Request) {
	slot := time.Now().UTC().Unix() / 10
	app.Logf(beatLevels[int(slot%int64(len(beatLevels)))], "ticker beat")
	w.WriteHeader(http.StatusNoContent)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ticker logs a beat every 10s from the supervisor's schedule,")
	fmt.Fprintln(w, "cycling TRACE, DEBUG, INFO, WARN, ERROR and FATAL.")
}

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tick", handleTick)
	mux.HandleFunc("/", handleIndex)
	wasihttp.Handle(mux.ServeHTTP)
}

// main is required to build, but the component's entry point is the exported wasi:http handler.
func main() {}
