package session

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueAndHash(t *testing.T) {
	token, hash, err := Issue()
	if err != nil {
		t.Fatal(err)
	}

	if token == "" || len(hash) != 64 || hash != Hash(token) {
		t.Errorf("token %q hash %q", token, hash)
	}

	if Equal(token, token) != true || Equal(token, token+"x") != false {
		t.Error("Equal did not compare tokens")
	}
}

func TestCookieLifecycle(t *testing.T) {
	manager := New("passport_session", time.Hour)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://passport.example/", nil)
	manager.Set(recorder, request, "token")

	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}

	cookie := cookies[0]
	if cookie.Name != "passport_session" || cookie.Value != "token" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 3600 {
		t.Errorf("cookie = %+v", cookie)
	}

	read := httptest.NewRequest(http.MethodGet, "https://passport.example/", nil)
	read.AddCookie(cookie)

	if got, ok := manager.Token(read); !ok || got != "token" {
		t.Errorf("token = %q %v", got, ok)
	}

	cleared := httptest.NewRecorder()
	manager.Clear(cleared, request)

	expired := cleared.Result().Cookies()[0]
	if expired.MaxAge != -1 || expired.Value != "" {
		t.Errorf("expired cookie = %+v", expired)
	}
}

func TestSecureAndSameOrigin(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://passport.example/", nil)
	if Secure(request) {
		t.Error("plain request marked secure")
	}

	request.TLS = &tls.ConnectionState{}
	if !Secure(request) {
		t.Error("TLS request not marked secure")
	}

	request = httptest.NewRequest(http.MethodPost, "http://passport.example/", nil)
	request.Header.Set("Origin", "https://passport.example")

	if !SameOrigin(request) {
		t.Error("same-origin request rejected")
	}

	request.Header.Set("Origin", "https://evil.example")

	if SameOrigin(request) {
		t.Error("cross-origin request accepted")
	}

	request = httptest.NewRequest(http.MethodPost, "http://passport.localhost/", nil)
	request.Header.Set("Origin", "http://127.0.0.1:5173")

	if !SameOrigin(request) {
		t.Error("loopback dev origin rejected")
	}
}
