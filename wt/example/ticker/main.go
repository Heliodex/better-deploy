package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	wasihttp "wt.packages/wasihttp"
)

// defaultDelayURL paces the beat loop: each GET takes about ten seconds, so one log line is emitted per round trip without any in-component timers.
const defaultDelayURL = "http://httpbin.org/delay/10"

func delayURL() string {
	if url := os.Getenv("DELAY_URL"); url != "" {
		return url
	}
	return defaultDelayURL
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ticker streams a log line every 10s on /beat")
}

// handleBeat holds the request open, emitting one log line and SSE event per delay round trip. A background goroutine cannot do this instead: component instances are per-request, and timers trap under wasmtime serve.
func handleBeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	fmt.Fprintln(w, "retry: 3000")
	fmt.Fprintln(w)
	client := wasihttp.NewClient()
	n := 0
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if err := beatOnce(client, w, n+1); err != nil {
			log.Printf("ticker beat failed: %v", err)
			return
		}
		n++
	}
}

func beatOnce(client *http.Client, w http.ResponseWriter, n int) error {
	resp, err := client.Get(delayURL())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	log.Printf("ticker beat %d", n)
	fmt.Fprintf(w, "event: beat\ndata: %d\n\n", n)
	return nil
}

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /beat", handleBeat)
	mux.HandleFunc("/", handleIndex)
	wasihttp.Handle(mux.ServeHTTP)
}

// main is required to build, but the component's entry point is the exported wasi:http handler.
func main() {}
