//go:build !wasip1

// Package wasihttp contains the helper functions for writing HTTP apps in Go.
// On wasip1 it maps requests to a wasi:http component export; on other targets it runs a normal net/http server so the same app code can be deployed natively.
package wasihttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/v2/httpmeta"
)

// Transport implements [http.RoundTripper] using net/http.
type Transport struct{}

// defaultClientTimeout matches the wasi:http client so callers see the same default on both backends.
const defaultClientTimeout = 30 * time.Second

// NewClient returns a new HTTP client with the same default timeout as the wasi:http client.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &Transport{},
		Timeout: defaultClientTimeout,
	}
}

// NewTransport returns an [http.RoundTripper] backed by net/http.
func NewTransport() http.RoundTripper {
	return &Transport{}
}

// RequestID returns the request identifier set by the host or an empty string.
func RequestID(r *http.Request) string {
	return r.Header.Get(HeaderRequestID)
}

// Send sends an HTTP request using net/http.
func Send(req *http.Request) (*http.Response, error) {
	return NewClient().Do(req)
}

// Get issues a GET request.
func Get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	return Send(req)
}

// Post issues a POST request with the given content type and body.
func Post(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return Send(req)
}

// RoundTrip executes a single HTTP transaction using net/http.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(req)
}

// normalize fills the request fields the supervisor normally supplies through forwarded headers.
func normalize(fn func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, host, _, remote := httpmeta.IncomingTarget(r.URL.RequestURI(), r.Header)
		if scheme == "" {
			scheme = "http"
		}

		if host == "" {
			host = r.Host
		}

		r.URL.Scheme = scheme
		r.URL.Host = host

		if host != "" {
			r.Host = host
		}

		if remote != "" {
			r.RemoteAddr = remote
		}

		if scheme == "https" && r.TLS == nil {
			r.TLS = &tls.ConnectionState{}
		}

		fn(w, r)
	})
}

// Handle sets the handler function and serves it until the process stops.
// The app calls it from init(), so it blocks there in a native build.
// It prints wasmtime's serve line so the supervisor discovers the port with the same code path as a component.
func Handle(fn func(http.ResponseWriter, *http.Request)) {
	addr := os.Getenv("WT_APP_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:0"
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("wasihttp: listen %s: %v", addr, err)
	}

	fmt.Fprintf(os.Stdout, "Serving HTTP on http://%s/\n", listener.Addr())

	server := &http.Server{
		Handler: normalize(fn),
		ReadHeaderTimeout: 5 * time.Second,
	}
	signals := make(chan os.Signal, 1)

	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-signals

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		defer cancel()

		_ = server.Shutdown(ctx)
	}()

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("wasihttp: serve: %v", err)
	}
}
