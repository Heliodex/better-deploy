package main

import (
	"fmt"
	"log"
	"net/http"

	wasihttp "wt.packages/wasihttp"
)

// handleTick is driven by the schedule in app.json, not by a client. Components
// under `wasmtime serve` only run while handling a request, so the supervisor
// fires this endpoint every ten seconds and the panel stamps each log line.
func handleTick(w http.ResponseWriter, r *http.Request) {
	log.Printf("ticker beat")
	w.WriteHeader(http.StatusNoContent)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ticker logs a beat every 10s from the supervisor's schedule")
}

func init() {
	// The supervisor stamps every line it captures, so the app adds no timestamp.
	log.SetFlags(0)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tick", handleTick)
	mux.HandleFunc("/", handleIndex)
	wasihttp.Handle(mux.ServeHTTP)
}

// main is required to build, but the component's entry point is the exported wasi:http handler.
func main() {}
