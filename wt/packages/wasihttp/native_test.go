//go:build !wasip1

package wasihttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleReturnsInTestBinary(t *testing.T) {
	done := make(chan struct{})

	go func() {
		Handle(func(http.ResponseWriter, *http.Request) {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle started a server inside a test binary")
	}
}

func TestNewClient(t *testing.T) {
	if client := NewClient(); client == nil || client.Transport == nil || client.Timeout != defaultClientTimeout {
		t.Errorf("client = %+v", client)
	}
}

func TestNormalizeFillsForwardedRequest(t *testing.T) {
	var seen *http.Request

	handler := normalize(func(_ http.ResponseWriter, r *http.Request) {
		seen = r
	})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/notes", nil)

	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "notes.example")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil {
		t.Fatal("handler was not called")
	}

	if seen.URL.Scheme != "https" || seen.URL.Host != "notes.example" || seen.Host != "notes.example" {
		t.Errorf("request target = %s://%s host %s", seen.URL.Scheme, seen.URL.Host, seen.Host)
	}

	if seen.RemoteAddr != "203.0.113.7" {
		t.Errorf("RemoteAddr = %q", seen.RemoteAddr)
	}

	if seen.TLS == nil {
		t.Error("TLS should be set for an https request")
	}
}

func TestRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	req.Header.Set(HeaderRequestID, "req-1")

	if got := RequestID(req); got != "req-1" {
		t.Errorf("RequestID = %q", got)
	}
}
