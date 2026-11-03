package app

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func publicRequest(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func TestPublicURL(t *testing.T) {
	t.Setenv("WT_PUBLIC_URL", "")

	request := publicRequest("http://passport.localhost/auth/login")
	if got := PublicURL(request); got != "http://passport.localhost" {
		t.Errorf("plain request = %q", got)
	}

	request.TLS = &tls.ConnectionState{}
	if got := PublicURL(request); got != "https://passport.localhost" {
		t.Errorf("TLS request = %q", got)
	}

	request = publicRequest("http://passport.localhost/auth/login")
	request.Header.Set("Origin", "https://passport.localhost")

	if got := PublicURL(request); got != "https://passport.localhost" {
		t.Errorf("same-origin Origin = %q", got)
	}

	request = publicRequest("http://passport.localhost/auth/login")
	request.Header.Set("Origin", "https://evil.example")

	if got := PublicURL(request); got != "http://passport.localhost" {
		t.Errorf("spoofed Origin = %q", got)
	}

	request = publicRequest("http://passport.localhost/auth/login")
	request.Header.Set("Origin", "http://127.0.0.1:5173")

	if got := PublicURL(request); got != "http://127.0.0.1:5173" {
		t.Errorf("loopback Origin = %q", got)
	}

	request = publicRequest("http://passport.localhost/auth/login")
	request.Header.Set("Referer", "https://passport.localhost/how")

	if got := PublicURL(request); got != "https://passport.localhost" {
		t.Errorf("Referer = %q", got)
	}
}

func TestPublicURLEnvOverride(t *testing.T) {
	t.Setenv("WT_PUBLIC_URL", "https://passport.example/")

	request := publicRequest("http://passport.localhost/")
	request.Header.Set("Origin", "http://127.0.0.1:5173")

	if got := PublicURL(request); got != "https://passport.example" {
		t.Errorf("env override = %q", got)
	}
}

func TestPublicURLRejectsInvalidEnv(t *testing.T) {
	t.Setenv("WT_PUBLIC_URL", "not-a-url")

	request := publicRequest("https://passport.example/")
	if got := PublicURL(request); got != "https://passport.example" {
		t.Errorf("invalid override fallback = %q", got)
	}
}
