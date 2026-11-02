package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

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

func TestProxyLeavesTextAndSSEAlone(t *testing.T) {
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
