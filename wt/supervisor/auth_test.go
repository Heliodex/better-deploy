package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAuthStore(t *testing.T) (*authStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".auth", "tokens.json")

	store, err := newAuthStore(path)
	if err != nil {
		t.Fatal(err)
	}

	return store, path
}

func TestAuthStorePersistsHashedTokens(t *testing.T) {
	store, path := testAuthStore(t)

	token, info, err := store.create("ci-notes", roleDeploy, []string{"notes", "notes"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if token == "" || info.Name != "ci-notes" || info.Role != roleDeploy || len(info.Apps) != 1 || info.Apps[0] != "notes" {
		t.Fatalf("created = %q %+v", token, info)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), token) {
		t.Fatal("auth file contains the bearer token")
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("auth file mode = %o, want 600", fileInfo.Mode().Perm())
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}

	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("auth dir mode = %o, want 700", dirInfo.Mode().Perm())
	}

	reloaded, err := newAuthStore(path)
	if err != nil {
		t.Fatal(err)
	}

	identity, ok := reloaded.identity(token)
	if !ok || identity.Name != "ci-notes" || identity.Role != roleDeploy || !identity.canAccessApp("notes") || identity.canAccessApp("other") {
		t.Errorf("reloaded identity = %+v, %v", identity, ok)
	}

	if err := reloaded.remove(info.ID); err != nil {
		t.Fatal(err)
	}

	if _, ok := reloaded.identity(token); ok {
		t.Error("removed token still authenticates")
	}
}

func TestAuthStoreRejectsExpiredAndCorruptTokens(t *testing.T) {
	store, path := testAuthStore(t)

	token, _, err := store.create("expired", roleRead, []string{"notes"}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := store.identity(token); ok {
		t.Error("expired token authenticated")
	}

	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := newAuthStore(path); err == nil {
		t.Error("corrupt auth file was accepted")
	}
}

func TestAuthTokenAdminOnlyAndRevocation(t *testing.T) {
	s := testSupervisor()
	store, _ := testAuthStore(t)
	s.auth = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/tokens", strings.NewReader(`{"name":"ci","role":"deploy","apps":["demo"]}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAuthTokens(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create token = %d: %s", rec.Code, rec.Body.String())
	}

	var created authTokenCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created.Token == "" || created.Apps[0] != "demo" {
		t.Fatalf("created = %+v", created)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/auth/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	s.handleAuthTokens(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("deploy token list = %d, want 403", rec.Code)
	}

	s.apps["demo"] = &app{name: "demo", cfg: appConfig{Host: "demo.localhost"}}
	s.apps["other"] = &app{name: "other", cfg: appConfig{Host: "other.localhost"}}

	for target, want := range map[string]int{
		"/apps/demo":    http.StatusOK,
		"/apps/other":   http.StatusForbidden,
		"/apps/missing": http.StatusForbidden,
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+created.Token)
		s.handleApp(rec, req)

		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, want)
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/auth/tokens/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAuthToken(rec, req, created.ID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete token = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/apps/demo", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	s.handleApp(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", rec.Code)
	}
}

func TestAppRolePermissions(t *testing.T) {
	read := authIdentity{Name: "monitor", Role: roleRead, Apps: []string{"notes"}}
	deploy := authIdentity{Name: "ci", Role: roleDeploy, Apps: []string{"notes"}}

	admin := authIdentity{Name: "operator", Role: roleAdmin, Apps: []string{"*"}}
	for _, test := range []struct {
		identity authIdentity
		method   string
		target   string
		action   string
		want     bool
	}{
		{read, http.MethodGet, "/apps/notes", "", true},
		{read, http.MethodGet, "/apps/notes/database", "database", false},
		{read, http.MethodPost, "/apps/notes/restart", "restart", false},
		{deploy, http.MethodPost, "/apps/notes", "", true},
		{deploy, http.MethodPost, "/apps/notes/deployments", "deployments", true},
		{deploy, http.MethodPost, "/apps/notes/deployments/id/cancel", "deployments/id/cancel", true},
		{deploy, http.MethodPost, "/apps/notes/restart", "restart", true},
		{deploy, http.MethodPost, "/apps/notes/maintenance", "maintenance", true},
		{deploy, http.MethodPost, "/apps/notes/rollback/id", "rollback/id", true},
		{deploy, http.MethodPost, "/apps/notes/rollback/id?backup=snap", "rollback/id", false},
		{deploy, http.MethodPatch, "/apps/notes/config", "config", false},
		{deploy, http.MethodGet, "/apps/notes/backups", "backups", false},
		{admin, http.MethodDelete, "/apps/notes", "", true},
	} {
		r := httptest.NewRequest(test.method, test.target, nil)
		if got := test.identity.canPerformAppAction(r, test.action); got != test.want {
			t.Errorf("%s %s %s = %v, want %v", test.identity.Role, test.method, test.action, got, test.want)
		}
	}
}

func TestLoginRateLimitAndScopedLogin(t *testing.T) {
	s := testSupervisor()
	store, _ := testAuthStore(t)
	s.auth = store

	token, _, err := store.create("ci", roleDeploy, []string{"notes"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	for range loginMaxFailures {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"wrong"}`))
		req.RemoteAddr = "192.0.2.5:1234"
		rec := httptest.NewRecorder()
		s.handleLogin(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failed login = %d, want 401", rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"wrong"}`))
	req.RemoteAddr = "192.0.2.5:1234"
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("limited login = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":"`+token+`"}`))
	req.RemoteAddr = "192.0.2.6:1234"
	rec = httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("scoped login = %d: %s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != token {
		t.Errorf("scoped login cookies = %+v", cookies)
	}
}

func TestAuditRecordsMutatingActions(t *testing.T) {
	s := testSupervisor()

	audit, err := newAuditStore(filepath.Join(t.TempDir(), ".audit", "audit.log"))
	if err != nil {
		t.Fatal(err)
	}

	s.audit = audit
	s.apps["demo"] = &app{name: "demo", cfg: appConfig{Host: "demo.localhost"}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/stop", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(requestIDHeader, "request-audit")
	s.handleApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", rec.Code, rec.Body.String())
	}

	entries, err := audit.tail(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("audit entries = %+v", entries)
	}

	entry := entries[0]
	if entry.Actor != "operator" || entry.Role != roleAdmin || entry.Action != "stop" || entry.App != "demo" || entry.Status != http.StatusOK || !entry.OK || entry.RequestID != "request-audit" {
		t.Errorf("audit entry = %+v", entry)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/audit", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleAudit(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"stop"`) {
		t.Errorf("audit API = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAppsFiltersScopedTokens(t *testing.T) {
	s := testSupervisor()
	store, _ := testAuthStore(t)
	s.auth = store

	token, _, err := store.create("monitor", roleRead, []string{"demo"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	s.apps["demo"] = &app{name: "demo", cfg: appConfig{Host: "demo.localhost"}}
	s.apps["other"] = &app{name: "other", cfg: appConfig{Host: "other.localhost"}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.handleApps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("apps = %d: %s", rec.Code, rec.Body.String())
	}

	var apps []appInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
		t.Fatal(err)
	}

	if len(apps) != 1 || apps[0].Name != "demo" {
		t.Errorf("scoped apps = %+v", apps)
	}
}
