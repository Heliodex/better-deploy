package main

import (
	"fmt"
	"net/http"

	wasihttp "wt.example/wasihttp"
)

func init() {
	wasihttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "hello from wt\nmethod=%s\npath=%s\n", r.Method, r.URL.Path)
	})
}

// main is required to build, but the component's entry point is the exported
// wasi:http handler.
func main() {}
