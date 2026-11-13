package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterPrunesExpiredAddresses(t *testing.T) {
	limiter := newLoginLimiter()
	start := time.Now()

	for i := range loginMaxTrackedAddresses {
		limiter.fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256), start)
	}

	if got := len(limiter.attempts); got != loginMaxTrackedAddresses {
		t.Fatalf("tracked = %d, want %d", got, loginMaxTrackedAddresses)
	}

	later := start.Add(loginWindow + time.Second)
	limiter.fail("192.0.2.1", later)

	if got := len(limiter.attempts); got != 1 {
		t.Errorf("tracked after expiry = %d, want 1", got)
	}
}

func TestLoginSetsHttpOnlyCookie(t *testing.T) {
	s := testSupervisor()
	s.cfg.trustedProxies = mustTrustedProxies(t, "127.0.0.1/32")
	handler := s.handler()

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"secret"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}

	cookie := rec.Result().Cookies()
	if len(cookie) != 1 {
		t.Fatalf("cookies = %+v", cookie)
	}

	auth := cookie[0]
	if auth.Name != "wt_token" || auth.Value != "secret" || !auth.HttpOnly || !auth.Secure || auth.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v", auth)
	}

	// The cookie authenticates the panel and event streams without exposing the token to scripts.
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.AddCookie(auth)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("cookie auth = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"wrong"}`))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}

	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("wrong token set cookies: %+v", cookies)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Errorf("logout cookies = %+v", cookies)
	}
}

func TestLoginIgnoresForwardedProtoFromUntrustedPeer(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"secret"}`))
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure {
		t.Errorf("untrusted forwarded proto set Secure: %+v", cookies)
	}
}
