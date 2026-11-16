package httpmeta

import (
	"net/http"
	"testing"
)

func TestIncomingTargetDefaults(t *testing.T) {
	if scheme, host, path, remote := IncomingTarget("/", http.Header{}); scheme != "http" || host != "" || path != "/" || remote != "" {
		t.Errorf("target = %q %q %q %q", scheme, host, path, remote)
	}
}

func TestIncomingTargetFallsBackToForwardedHeaders(t *testing.T) {
	headers := http.Header{}

	headers.Set("X-Forwarded-Host", "notes.example.com")
	headers.Set("X-Forwarded-Proto", "https")
	headers.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")

	if scheme, host, path, remote := IncomingTarget("/v1/notes", headers); scheme != "https" || host != "notes.example.com" || path != "/v1/notes" || remote != "198.51.100.7" {
		t.Errorf("target = %q %q %q %q", scheme, host, path, remote)
	}
}

func TestIncomingTargetUsesSpinHeaders(t *testing.T) {
	headers := http.Header{}

	headers.Set("Spin-Full-Url", "https://notes.example.com/v1/notes?limit=2")
	headers.Set("Spin-Client-Addr", "203.0.113.9:4242")
	headers.Set("Host", "ignored.example.com")

	if scheme, host, path, remote := IncomingTarget("/fallback", headers); scheme != "https" || host != "notes.example.com" || path != "/v1/notes?limit=2" || remote != "203.0.113.9:4242" {
		t.Errorf("target = %q %q %q %q", scheme, host, path, remote)
	}
}
