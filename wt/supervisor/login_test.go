package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginSetsHttpOnlyCookie(t *testing.T) {
	s := testSupervisor()
	handler := s.handler()

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"secret"}`))
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
