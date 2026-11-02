package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
)

func mustTrustedProxies(t *testing.T, value string) []netip.Prefix {
	t.Helper()

	prefixes, err := parseTrustedProxies(value)
	if err != nil {
		t.Fatal(err)
	}

	return prefixes
}

func TestParseTrustedProxies(t *testing.T) {
	prefixes, err := parseTrustedProxies("127.0.0.1, 10.0.0.0/8 192.168.0.0/16,10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}

	if len(prefixes) != 3 {
		t.Fatalf("prefixes = %v, want three unique entries", prefixes)
	}

	for _, value := range []string{"127.0.0.1/33", "not-an-ip", "10.0.0.0/x", "*"} {
		if _, err := parseTrustedProxies(value); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}

func TestForwardedInfoTrustsOnlyConfiguredProxies(t *testing.T) {
	s := &supervisor{cfg: config{trustedProxies: mustTrustedProxies(t, "10.0.0.0/8")}}
	r := httptest.NewRequest(http.MethodGet, "http://internal:8080/hello?x=1", nil)
	r.Host = "internal:8080"
	r.RemoteAddr = "10.0.0.2:5555"
	r.Header.Set("X-Forwarded-Host", "Notes.Example.com:8443")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.5, 10.0.0.3")

	info := s.forwardedInfo(r)
	if !info.Trusted || info.Host != "notes.example.com" || info.Authority != "notes.example.com:8443" || info.Proto != "https" || info.ClientIP != "203.0.113.5" {
		t.Errorf("trusted forwarded info = %+v", info)
	}

	if s.effectiveHost(r) != "notes.example.com" {
		t.Errorf("effective host = %q", s.effectiveHost(r))
	}

	r.RemoteAddr = "198.51.100.9:1234"

	info = s.forwardedInfo(r)
	if info.Trusted || info.Host != "internal" || info.Authority != "internal:8080" || info.Proto != "http" || info.ClientIP != "198.51.100.9" {
		t.Errorf("untrusted forwarded info = %+v", info)
	}
}

func TestForwardedClientIPSkipsTrustedHops(t *testing.T) {
	s := &supervisor{cfg: config{trustedProxies: mustTrustedProxies(t, "10.0.0.0/8,192.168.0.0/16")}}
	for _, test := range []struct {
		chain string
		want  string
	}{
		{"203.0.113.5, 10.0.0.3, 192.168.1.1", "203.0.113.5"},
		{"203.0.113.5, 198.51.100.7", "198.51.100.7"},
		{"10.0.0.9, 10.0.0.3", "10.0.0.9"},
		{"unknown, 203.0.113.5:1234", "203.0.113.5"},
	} {
		if got := s.forwardedClientIP([]string{test.chain}, "10.0.0.2"); got != test.want {
			t.Errorf("chain %q = %q, want %q", test.chain, got, test.want)
		}
	}
}

func TestProxyRewritesForwardedHeadersCleanly(t *testing.T) {
	type seen struct {
		host, forwardedHost, forwardedProto, forwardedFor, realIP, forwarded, spinClient string
	}

	got := make(chan seen, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{
			host:           r.Host,
			forwardedHost:  r.Header.Get("X-Forwarded-Host"),
			forwardedProto: r.Header.Get("X-Forwarded-Proto"),
			forwardedFor:   r.Header.Get("X-Forwarded-For"),
			realIP:         r.Header.Get("X-Real-IP"),
			forwarded:      r.Header.Get("Forwarded"),
			spinClient:     r.Header.Get(spinClientAddrHeader),
		}

		w.WriteHeader(http.StatusNoContent)
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

	s := &supervisor{cfg: config{trustedProxies: mustTrustedProxies(t, "192.0.2.0/24")}}
	proxy := s.newProxy(&app{name: "test", port: port})
	req := httptest.NewRequest(http.MethodGet, "http://internal:8080/hello", nil)
	req.Host = "internal:8080"
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-Host", "notes.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.5")
	req.Header.Set("Forwarded", `for=1.1.1.1`)

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("proxy = %d: %s", rec.Code, rec.Body.String())
	}

	request := <-got
	if request.host != "notes.example.com" || request.forwardedHost != "notes.example.com" || request.forwardedProto != "https" {
		t.Errorf("host headers = %+v", request)
	}

	if request.forwardedFor != "203.0.113.5, 192.0.2.1" || request.realIP != "203.0.113.5" || request.spinClient != "203.0.113.5" {
		t.Errorf("client headers = %+v", request)
	}

	if request.forwarded != "" {
		t.Errorf("Forwarded header = %q", request.forwarded)
	}
}

func TestHandlerRoutesByTrustedForwardedHost(t *testing.T) {
	s := testSupervisor()
	s.cfg.trustedProxies = mustTrustedProxies(t, "127.0.0.1/32")
	a := &app{name: "notes", cfg: appConfig{Host: "notes.localhost"}}
	s.apps["notes"] = a
	s.hosts["notes.localhost"] = a

	req := httptest.NewRequest(http.MethodGet, "http://internal:8080/apps", nil)
	req.Host = "internal:8080"
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("untrusted admin route = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "http://internal:8080/apps", nil)
	req.Host = "internal:8080"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Forwarded-Host", "notes.localhost")

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("trusted forwarded host = %d, want the app route's 503", rec.Code)
	}
}
