//go:build !wasip1

package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wt.example/passport/contract"
	"github.com/heliodex/better-deploy/wt/packages/app"
	"github.com/heliodex/better-deploy/wt/packages/oauth"
	"github.com/heliodex/better-deploy/wt/packages/session"
	"github.com/heliodex/better-deploy/wt/packages/storage"
)

// testIssuer serves the discovery document a handler test needs for login and logout.
func testIssuer(t *testing.T) string {
	t.Helper()

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		document, err := json.Marshal(map[string]string{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"end_session_endpoint":   server.URL + "/endsession",
		})
		if err != nil {
			t.Errorf("encode discovery: %v", err)
			return
		}

		_, _ = w.Write(document)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// testApp builds the app against an in-memory storage service and a local provider.
func testApp(t *testing.T) *passportApp {
	t.Helper()
	t.Setenv("WT_PUBLIC_URL", "https://passport.example")
	t.Setenv("OAUTH_ISSUER", testIssuer(t))

	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Close)

	application, err := newPassportApp(server.NewClient(server.Client()), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	if err := application.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	return application
}

// seedSession stores a session directly so a test can act as a signed-in user.
func seedSession(t *testing.T, store *storage.Client, token string, user oauth.Identity) {
	t.Helper()

	now := time.Now().UTC()

	_, err := store.Exec(t.Context(), `insert into sessions (id, user_sub, user_name, user_email, user_username, user_picture, user_zoneinfo,
		access_token, refresh_token, id_token, created_at, expires_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.Hash(token), user.Subject, user.Name, user.Email, user.Username, user.Picture, user.Zoneinfo,
		"access", "refresh", "id", now.Format(app.TimestampFormat), now.Add(sessionTTL).Format(app.TimestampFormat))
	if err != nil {
		t.Fatal(err)
	}
}

func do(handler http.Handler, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}

	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

func TestCheckinsRequireSession(t *testing.T) {
	application := testApp(t)
	handler := application.handler()

	recorder := do(handler, http.MethodPost, "/api/checkins", `{"message":"hello"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("create without session = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(handler, http.MethodGet, "/api/checkins", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", recorder.Code, recorder.Body.String())
	}

	var listed contract.CheckinsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}

	if len(listed.Checkins) != 0 {
		t.Errorf("checkins = %+v", listed.Checkins)
	}
}

func TestCheckinOwnership(t *testing.T) {
	application := testApp(t)
	handler := application.handler()
	seedSession(t, application.store, "alice-token", oauth.Identity{Subject: "alice", Name: "Alice"})
	seedSession(t, application.store, "bob-token", oauth.Identity{Subject: "bob", Name: "Bob"})

	recorder := do(handler, http.MethodPost, "/api/checkins", `{"message":"  hello from alice  "}`, &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body.String())
	}

	var created contract.Checkin
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created.Message != "hello from alice" || created.AuthorSub != "alice" || !created.Mine {
		t.Errorf("created = %+v", created)
	}

	recorder = do(handler, http.MethodGet, "/api/checkins", "", &http.Cookie{Name: sessionCookie, Value: "bob-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", recorder.Code, recorder.Body.String())
	}

	var listed contract.CheckinsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}

	if len(listed.Checkins) != 1 || listed.Checkins[0].Mine {
		t.Errorf("bob's view = %+v", listed.Checkins)
	}

	recorder = do(handler, http.MethodDelete, "/api/checkins/"+created.ID, "", &http.Cookie{Name: sessionCookie, Value: "bob-token"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("delete other = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(handler, http.MethodDelete, "/api/checkins/"+created.ID, "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete own = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCheckinValidationAndOrigin(t *testing.T) {
	application := testApp(t)
	handler := application.handler()
	seedSession(t, application.store, "alice-token", oauth.Identity{Subject: "alice", Name: "Alice"})
	cookie := &http.Cookie{Name: sessionCookie, Value: "alice-token"}

	recorder := do(handler, http.MethodPost, "/api/checkins", `{"message":"   "}`, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("blank = %d: %s", recorder.Code, recorder.Body.String())
	}

	var body contract.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.Code != "invalid" || body.Field == nil || *body.Field != "message" {
		t.Errorf("validation body = %+v", body)
	}

	tooLong := strings.Repeat("a", maxMessage+1)

	recorder = do(handler, http.MethodPost, "/api/checkins", `{"message":"`+tooLong+`"}`, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("too long = %d: %s", recorder.Code, recorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/checkins", strings.NewReader(`{"message":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	request.AddCookie(cookie)

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionResponseAndLogout(t *testing.T) {
	application := testApp(t)
	handler := application.handler()
	seedSession(t, application.store, "alice-token", oauth.Identity{Subject: "alice", Name: "Alice", Email: "alice@example.com"})

	recorder := do(handler, http.MethodGet, "/api/session", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("session = %d: %s", recorder.Code, recorder.Body.String())
	}

	var authSession contract.SessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &authSession); err != nil {
		t.Fatal(err)
	}

	if !authSession.Authenticated || authSession.UserName != "Alice" || authSession.ClientID != "interactive.public" {
		t.Errorf("session = %+v", authSession)
	}

	recorder = do(handler, http.MethodPost, "/api/logout", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", recorder.Code, recorder.Body.String())
	}

	var logout contract.LogoutResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &logout); err != nil {
		t.Fatal(err)
	}

	endSession, err := url.Parse(logout.EndSessionURL)
	if err != nil || endSession.Path != "/endsession" || endSession.Query().Get("id_token_hint") != "id" {
		t.Errorf("end session URL = %q", logout.EndSessionURL)
	}

	recorder = do(handler, http.MethodGet, "/api/session", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if err := json.Unmarshal(recorder.Body.Bytes(), &authSession); err != nil {
		t.Fatal(err)
	}

	if authSession.Authenticated {
		t.Error("session survived logout")
	}
}

func TestLoginRedirectsWithPublicOrigin(t *testing.T) {
	application := testApp(t)
	handler := application.handler()

	recorder := do(handler, http.MethodGet, "/auth/login", "")
	if recorder.Code != http.StatusFound {
		t.Fatalf("login = %d: %s", recorder.Code, recorder.Body.String())
	}

	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if location.Path != "/authorize" {
		t.Errorf("login location = %s", location)
	}

	if location.Query().Get("redirect_uri") != "https://passport.example/auth/callback/" {
		t.Errorf("redirect_uri = %q", location.Query().Get("redirect_uri"))
	}

	if location.Query().Get("code_challenge_method") != "S256" || location.Query().Get("state") == "" || location.Query().Get("nonce") == "" {
		t.Errorf("authorise query = %v", location.Query())
	}
}
