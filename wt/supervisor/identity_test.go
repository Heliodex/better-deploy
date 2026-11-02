package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testIdentitySupervisor(t *testing.T) (*supervisor, *app, identityUserInfo) {
	t.Helper()
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	store, err := newIdentityStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	s.identity = store
	blobs, err := newBlobStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = blobs
	a := &app{name: "demo", cfg: appConfig{Host: "demo.localhost", Identity: identityModePlatform}}
	s.apps["demo"] = a
	s.hosts["demo.localhost"] = a
	user, err := store.createUser("ada@example.com", "Ada", "correct horse battery", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	return s, a, user
}

func identityCall(t *testing.T, s *supervisor, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(identityCSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	s.serveIdentityApp(rec, req)
	return rec
}

func TestIdentityStorePasswordAndSessions(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	store, err := newIdentityStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.createUser("ada@example.com", "Ada", "correct horse battery", []string{"admins", "admins", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(user.Groups) != 2 {
		t.Errorf("groups = %v", user.Groups)
	}
	if _, err := store.createUser("ada@example.com", "Ada", "correct horse battery", nil); err == nil {
		t.Error("duplicate email was accepted")
	}
	if _, err := store.authenticate("ada@example.com", "wrong password"); err == nil {
		t.Error("wrong password was accepted")
	}
	if _, err := store.authenticate("ada@example.com", "correct horse battery"); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	token, session, err := store.newSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.CSRF == "" {
		t.Error("session has no CSRF token")
	}
	got, gotUser, err := store.session(token)
	if err != nil || gotUser.ID != user.ID || got.CSRF != session.CSRF {
		t.Errorf("session = %+v %+v %v", got, gotUser, err)
	}
	if err := store.deleteSession(token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.session(token); err == nil {
		t.Error("deleted session still resolves")
	}

	reloaded, err := newIdentityStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.authenticate("ada@example.com", "correct horse battery"); err != nil {
		t.Errorf("password did not survive reload: %v", err)
	}
}

func TestIdentityLoginMeLogout(t *testing.T) {
	s, _, _ := testIdentitySupervisor(t)

	rec := identityCall(t, s, http.MethodPost, identityAppPrefix+"/login", `{"email":"ada@example.com","password":"correct horse battery"}`, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != identityCookieName {
		t.Fatalf("login cookies = %v", cookies)
	}
	var me identityMeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User.Email != "ada@example.com" || me.CSRFToken == "" {
		t.Errorf("login response = %+v", me)
	}

	rec = identityCall(t, s, http.MethodGet, identityAppPrefix+"/me", "", cookies[0], "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Errorf("me body = %s", rec.Body.String())
	}

	rec = identityCall(t, s, http.MethodPost, identityAppPrefix+"/logout", "", cookies[0], "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("logout without CSRF = %d, want 403", rec.Code)
	}

	rec = identityCall(t, s, http.MethodPost, identityAppPrefix+"/logout", "", cookies[0], me.CSRFToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", rec.Code, rec.Body.String())
	}

	rec = identityCall(t, s, http.MethodGet, identityAppPrefix+"/me", "", cookies[0], "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", rec.Code)
	}
}

func TestIdentityPasswordChangeAndProxyHeaders(t *testing.T) {
	s, _, user := testIdentitySupervisor(t)
	token, session, err := s.identity.newSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: identityCookieName, Value: token}

	rec := identityCall(t, s, http.MethodPost, identityAppPrefix+"/password",
		`{"currentPassword":"correct horse battery","newPassword":"another long password"}`, cookie, session.CSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("password = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.identity.authenticate("ada@example.com", "another long password"); err != nil {
		t.Errorf("new password rejected: %v", err)
	}
	if _, _, err := s.identity.session(token); err == nil {
		t.Error("old session survived a password change")
	}
	newCookies := rec.Result().Cookies()
	if len(newCookies) == 0 {
		t.Fatal("password change did not set a new session")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(newCookies[0])
	req.Header.Set("X-WT-User-Id", "spoof")
	s.identityProxyHeaders(req, "demo", true)
	if got := req.Header.Get(identityUserIDHeader); got != user.ID {
		t.Errorf("injected user id = %q, want %q", got, user.ID)
	}
	if got := req.Header.Get(identityUserSignatureHeader); got == "" {
		t.Error("injected user has no signature")
	}
	want := identityUserSignature(s.identitySecretFor("demo"), user.ID, "ada@example.com", "Ada", "admins")
	if got := req.Header.Get(identityUserSignatureHeader); got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
	if _, err := req.Cookie(identityCookieName); err == nil {
		t.Error("proxied request still carries the supervisor session cookie")
	}
}

func TestIdentityAdminUsers(t *testing.T) {
	s, _, _ := testIdentitySupervisor(t)
	body := `{"email":"grace@example.com","name":"Grace","password":"a long password","groups":["beta"]}`
	req := httptest.NewRequest(http.MethodPost, "/identity/users", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleIdentityUsers(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d: %s", rec.Code, rec.Body.String())
	}
	var user identityUserInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.Email != "grace@example.com" || len(user.Groups) != 1 {
		t.Errorf("created = %+v", user)
	}

	req = httptest.NewRequest(http.MethodGet, "/identity/users", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleIdentityUsers(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "grace@example.com") {
		t.Errorf("list users = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/identity/users/"+user.ID+"/password", strings.NewReader(`{"password":"reset password here"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleIdentityUser(rec, req, user.ID+"/password")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset password = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.identity.authenticate("grace@example.com", "reset password here"); err != nil {
		t.Errorf("reset password rejected: %v", err)
	}
}

func TestIdentitySignatureVector(t *testing.T) {
	if got := identityUserSignature("identity-secret", "usr_1", "ada@example.com", "Ada", "admins,beta"); got != "4eb999754e66af1d072d352fd3504b1fb6f404e7bfa414a0aec935fd5eebea49" {
		t.Errorf("identity signature = %s", got)
	}
}

func TestIdentityProxyRouting(t *testing.T) {
	s, _, _ := testIdentitySupervisor(t)
	req := httptest.NewRequest(http.MethodPost, "http://demo.localhost/_wt/identity/login", strings.NewReader(`{"email":"ada@example.com","password":"correct horse battery"}`))
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied login = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("proxied login set no cookie")
	}
	req = httptest.NewRequest(http.MethodGet, "http://demo.localhost"+identityAppPrefix+"/me", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	s.handleProxy(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Errorf("proxied me = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIdentityConfigValidation(t *testing.T) {
	if _, err := normalizeAppConfig(appConfig{Name: "demo", Identity: identityModePlatform}, "demo", "localhost", tenantTrusted); err != nil {
		t.Errorf("platform identity rejected: %v", err)
	}
	if _, err := normalizeAppConfig(appConfig{Name: "demo", Identity: "oauth"}, "demo", "localhost", tenantTrusted); err == nil {
		t.Error("unknown identity mode accepted")
	}
}

func TestIdentitySessionsExpire(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	store, err := newIdentityStore(s.cfg.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.createUser("ada@example.com", "Ada", "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	token, session, err := store.newSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.sessions[session.TokenHash].ExpiresAt = time.Now().Add(-time.Second)
	store.mu.Unlock()
	if _, _, err := store.session(token); err == nil {
		t.Error("expired session was accepted")
	}
	if _, err := filepath.Abs(store.dir); err != nil || !strings.Contains(store.dir, ".identity") {
		t.Errorf("identity dir = %q", store.dir)
	}
}
