//go:build !wasip1

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wt.example/passport/contract"
	"wt.packages/app"
	"wt.packages/storage"
)

// testApp builds the app against an in-memory storage service and a test HTTP client.
func testApp(t *testing.T) *passportApp {
	t.Helper()
	t.Setenv("OAUTH_BASE_URL", "https://passport.example")
	t.Setenv("OAUTH_ISSUER", "https://issuer.example")

	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Close)

	application := newPassportApp(server.NewClient(server.Client()), server.Client())
	if err := application.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	return application
}

// seedSession stores a session directly so a test can act as a signed-in user.
func seedSession(t *testing.T, store *storage.Client, token string, user profile) {
	t.Helper()

	now := time.Now().UTC()

	_, err := store.Exec(t.Context(), `insert into sessions (id, user_sub, user_name, user_email, user_username, user_picture, user_zoneinfo,
		access_token, refresh_token, id_token, created_at, expires_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token), user.Sub, user.Name, user.Email, user.Username, user.Picture, user.Zoneinfo,
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
	seedSession(t, application.store, "alice-token", profile{Sub: "alice", Name: "Alice"})
	seedSession(t, application.store, "bob-token", profile{Sub: "bob", Name: "Bob"})

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
	seedSession(t, application.store, "alice-token", profile{Sub: "alice", Name: "Alice"})
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
	seedSession(t, application.store, "alice-token", profile{Sub: "alice", Name: "Alice", Email: "alice@example.com"})

	recorder := do(handler, http.MethodGet, "/api/session", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("session = %d: %s", recorder.Code, recorder.Body.String())
	}

	var session contract.SessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	if !session.Authenticated || session.UserName != "Alice" || session.ClientID != "interactive.public" {
		t.Errorf("session = %+v", session)
	}

	recorder = do(handler, http.MethodPost, "/api/logout", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = do(handler, http.MethodGet, "/api/session", "", &http.Cookie{Name: sessionCookie, Value: "alice-token"})
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	if session.Authenticated {
		t.Error("session survived logout")
	}
}

// signedToken builds an RS256 ID token with the given claims.
func signedToken(t *testing.T, key *rsa.PrivateKey, kid string, claims oauthClaims) string {
	t.Helper()

	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// jwksServer serves a JWKS document for one test key.
func jwksServer(t *testing.T, key *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()

	document, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	t.Cleanup(server.Close)

	return server
}

func TestBaseURLUsesBrowserOrigin(t *testing.T) {
	t.Setenv("OAUTH_BASE_URL", "")

	client := newOAuthClient(serverClient(t))

	request := httptest.NewRequest(http.MethodPost, "http://internal/auth/login", nil)
	request.Host = "passport.example"
	request.Header.Set("Origin", "https://passport.example")

	if got := client.baseURL(request); got != "https://passport.example" {
		t.Errorf("origin base URL = %q", got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://passport.localhost/auth/login", nil)
	request.Host = "passport.localhost"
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	request.Header.Set("Referer", "http://127.0.0.1:5173/")

	if got := client.baseURL(request); got != "http://127.0.0.1:5173" {
		t.Errorf("loopback base URL = %q", got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://passport.localhost/auth/login", nil)
	request.Host = "passport.localhost"
	request.Header.Set("Origin", "https://evil.example")

	if got := client.baseURL(request); got != "http://passport.localhost" {
		t.Errorf("untrusted origin base URL = %q", got)
	}
}

// serverClient is a plain client for helpers that do not make requests.
func serverClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{Timeout: time.Second}
}

func TestVerifyIDToken(t *testing.T) {
	t.Setenv("OAUTH_ISSUER", "https://issuer.example")
	t.Setenv("OAUTH_CLIENT_ID", "interactive.public")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	server := jwksServer(t, &key.PublicKey, "test-key")
	client := newOAuthClient(server.Client())

	now := time.Now().Unix()
	claims := oauthClaims{
		Issuer:   "https://issuer.example",
		Audience: "interactive.public",
		Expires:  now + 300,
		Issued:   now,
		Nonce:    "nonce",
		Sub:      "alice",
		Name:     "Alice Example",
		Email:    "alice@example.com",
	}
	token := signedToken(t, key, "test-key", claims)

	verified, err := client.verifyIDToken(t.Context(), providerMetadata{JWKSUri: server.URL}, token, "nonce")
	if err != nil {
		t.Fatal(err)
	}

	user := verified.profile()
	if user.Sub != "alice" || user.Name != "Alice Example" || user.Username != "alice" {
		t.Errorf("profile = %+v", user)
	}

	if _, err := client.verifyIDToken(t.Context(), providerMetadata{JWKSUri: server.URL}, token, "other"); err == nil {
		t.Error("wrong nonce was accepted")
	}

	expired := claims

	expired.Expires = now - 300
	if _, err := client.verifyIDToken(t.Context(), providerMetadata{JWKSUri: server.URL}, signedToken(t, key, "test-key", expired), "nonce"); err == nil {
		t.Error("expired token was accepted")
	}

	wrongAudience := claims

	wrongAudience.Audience = "someone-else"
	if _, err := client.verifyIDToken(t.Context(), providerMetadata{JWKSUri: server.URL}, signedToken(t, key, "test-key", wrongAudience), "nonce"); err == nil {
		t.Error("wrong audience was accepted")
	}
}
