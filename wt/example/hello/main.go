package main

import (
	"fmt"
	"net/http"

	wasihttp "github.com/heliodex/better-deploy/wt/packages/wasihttp"
)

func init() {
	wasihttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("spin") == "1" {
			for { //nolint:revive,staticcheck // The spin endpoint intentionally burns CPU for the timeout test.
			}
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "hello from wt\nmethod=%s\npath=%s\n", r.Method, r.URL.Path) //nolint:gosec // The values are request metadata written as plain text.
	})
}

// main is required to build, but the component's entry point is the exported wasi:http handler.
func main() {}
