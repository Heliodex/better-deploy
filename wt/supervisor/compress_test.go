package main

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testProxyPort returns the port of a test backend.
func testProxyPort(t *testing.T, backend *httptest.Server) int {
	t.Helper()

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	return port
}

func TestCompressibleMediaType(t *testing.T) {
	for mediaType, want := range map[string]bool{
		"application/json":         true,
		"application/x-ndjson":     true,
		"application/ld+json":      true,
		"text/html":                true,
		"text/csv":                 true,
		"image/svg+xml":            true,
		"text/event-stream":        false,
		"image/png":                false,
		"application/octet-stream": false,
	} {
		if got := compressibleMediaType(mediaType); got != want {
			t.Errorf("compressibleMediaType(%q) = %v, want %v", mediaType, got, want)
		}
	}
}

// TestProxyStreamsCompressedNDJSON checks that a compressed stream still delivers each flushed line before the app finishes.
func TestProxyStreamsCompressedNDJSON(t *testing.T) {
	release := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"line":1}`+"\n")
		_ = http.NewResponseController(w).Flush()

		<-release

		_, _ = io.WriteString(w, `{"line":2}`+"\n")
	}))
	defer backend.Close()
	defer close(release)

	s := testSupervisor()
	proxy := s.newProxy(&app{name: "demo", port: testProxyPort(t, backend)})

	front := httptest.NewServer(proxy)
	defer front.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}

	lines := make(chan string, 1)

	go func() {
		reader, err := gzip.NewReader(resp.Body)
		if err != nil {
			lines <- "error: " + err.Error()
			return
		}

		line, _ := bufio.NewReader(reader).ReadString('\n')
		lines <- line
	}()

	select {
	case line := <-lines:
		if line != `{"line":1}`+"\n" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first NDJSON line was held back by compression")
	}
}

func TestProxyCompressesText(t *testing.T) {
	payload := strings.Repeat("plain text ", 100)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, payload)
	}))
	defer backend.Close()

	s := testSupervisor()
	proxy := s.newProxy(&app{name: "demo", port: testProxyPort(t, backend)})
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/text", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("text was not compressed: %v", rec.Header())
	}
}

func TestProxyCompressesJSON(t *testing.T) {
	payload := `{"data":"` + strings.Repeat("x", 1024) + `"}`
	seenAccept := ""

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer backend.Close()

	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	proxy := s.newProxy(&app{name: "demo", port: port})
	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/data", nil)
	req.Host = "demo.localhost"
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy = %d: %s", rec.Code, rec.Body.String())
	}

	if seenAccept != "identity" {
		t.Errorf("target Accept-Encoding = %q, want identity", seenAccept)
	}

	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" {
		t.Errorf("compression headers = %v", rec.Header())
	}

	reader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != payload {
		t.Errorf("decompressed body differs, got %d bytes want %d", len(body), len(payload))
	}
}

func TestProxyLeavesSSEAlone(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
	}))
	defer backend.Close()

	parsed, _ := url.Parse(backend.URL)
	port, _ := strconv.Atoi(parsed.Port())
	s := testSupervisor()
	proxy := s.newProxy(&app{name: "demo", port: port})

	req := httptest.NewRequest(http.MethodGet, "http://demo.localhost/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("SSE was compressed: %v", rec.Header())
	}

	if rec.Body.String() != "data: hello\n\n" {
		t.Errorf("SSE body = %q", rec.Body.String())
	}
}
