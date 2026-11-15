package oauth

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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type providerFixture struct {
	server *httptest.Server
	key *rsa.PrivateKey
	clientID, clientSecret, kid string
	tokenForm url.Values
	keyMu sync.RWMutex
}

func TestPKCE(t *testing.T) {
	verifier, challenge, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}

	if sum := sha256.Sum256([]byte(verifier)); challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Errorf("challenge = %q", challenge)
	}

	if len(verifier) < 43 {
		t.Errorf("verifier too short: %d", len(verifier))
	}
}

func fixtureClient(t *testing.T, fixture *providerFixture) *Client {
	t.Helper()

	client, err := New(Config{
		Issuer: fixture.server.URL,
		ClientID: fixture.clientID,
		ClientSecret: fixture.clientSecret,
		HTTPClient: fixture.server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return client
}

// setSigningMaterial rotates the key pair used by the token and JWKS endpoints.
func (f *providerFixture) setSigningMaterial(key *rsa.PrivateKey, kid string) {
	f.keyMu.Lock()

	defer f.keyMu.Unlock()

	f.key, f.kid = key, kid
}

// signingMaterial returns the current key pair under the rotation lock.
func (f *providerFixture) signingMaterial() (*rsa.PrivateKey, string) {
	f.keyMu.RLock()

	defer f.keyMu.RUnlock()

	return f.key, f.kid
}

func signedTestToken(t *testing.T, key *rsa.PrivateKey, kid string, value claims) string {
	t.Helper()

	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(value)
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

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	fixture := &providerFixture{
		key: key,
		clientID: "client",
		clientSecret: "secret",
		kid: "test-key",
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeTestJSON(t, w, map[string]string{
				"authorization_endpoint": fixture.server.URL + "/authorize",
				"end_session_endpoint": fixture.server.URL + "/endsession",
				"issuer": fixture.server.URL,
				"jwks_uri": fixture.server.URL + "/jwks",
				"token_endpoint": fixture.server.URL + "/token",
				"userinfo_endpoint": fixture.server.URL + "/userinfo",
			})
		case "/jwks":
			currentKey, kid := fixture.signingMaterial()

			writeTestJSON(t, w, map[string]any{
				"keys": []map[string]string{
					{
						"kty": "RSA",
						"kid": kid,
						"n": base64.RawURLEncoding.EncodeToString(currentKey.N.Bytes()),
						"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(currentKey.E)).Bytes()),
					},
				},
			})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}

			fixture.tokenForm = r.PostForm

			if _, password, ok := r.BasicAuth(); !ok || password != fixture.clientSecret {
				http.Error(w, "bad client auth", http.StatusUnauthorized)
				return
			}

			currentKey, kid := fixture.signingMaterial()
			token := signedTestToken(t, currentKey, kid, claims{
				Issuer: fixture.server.URL,
				Audience: fixture.clientID,
				Expires: time.Now().Add(time.Hour).Unix(),
				Issued: time.Now().Unix(),
				Nonce: "nonce",
				Subject: "alice",
				Name: "Alice Example",
				Email: "alice@example.com",
			})

			writeTestJSON(t, w, map[string]any{
				"access_token": "access",
				"expires_in": 3600,
				"id_token": token,
				"token_type": "Bearer",
			})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer access" {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			writeTestJSON(t, w, map[string]any{
				"email": "alice@example.com",
				"name": "Alice Example",
				"preferred_username": "alice",
				"sub": "alice",
			})
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(fixture.server.Close)
	return fixture
}

func TestDiscoverAndAuthoriseURL(t *testing.T) {
	fixture := newProviderFixture(t)
	client := fixtureClient(t, fixture)
	provider, err := client.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if provider.AuthorisationEndpoint != fixture.server.URL+"/authorize" || provider.JWKSUri != fixture.server.URL+"/jwks" {
		t.Errorf("provider = %+v", provider)
	}

	authoriseURL, err := client.AuthoriseURL(t.Context(), "https://app.example/auth/callback/", "state", "nonce", "challenge")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := url.Parse(authoriseURL)
	if err != nil {
		t.Fatal(err)
	}

	if query := parsed.Query(); parsed.Host != strings.TrimPrefix(fixture.server.URL, "http://") || query.Get("state") != "state" || query.Get("nonce") != "nonce" || query.Get("code_challenge") != "challenge" || query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != "https://app.example/auth/callback/" || query.Get("client_id") != fixture.clientID {
		t.Errorf("authorise URL = %s", authoriseURL)
	}
}

func TestEndSessionURL(t *testing.T) {
	fixture := newProviderFixture(t)
	client := fixtureClient(t, fixture)
	endSession, err := client.EndSessionURL(t.Context(), "id-token", "https://app.example/")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := url.Parse(endSession)
	if err != nil {
		t.Fatal(err)
	}

	if query := parsed.Query(); query.Get("client_id") != fixture.clientID || query.Get("id_token_hint") != "id-token" || query.Get("post_logout_redirect_uri") != "https://app.example/" {
		t.Errorf("end session URL = %s", endSession)
	}
}

func TestExchange(t *testing.T) {
	fixture := newProviderFixture(t)
	client := fixtureClient(t, fixture)
	tokens, err := client.Exchange(t.Context(), "code", "https://app.example/auth/callback/", "verifier")
	if err != nil {
		t.Fatal(err)
	}

	if tokens.AccessToken != "access" || tokens.IDToken == "" || tokens.ExpiresIn != 3600 {
		t.Errorf("tokens = %+v", tokens)
	}

	if fixture.tokenForm.Get("code") != "code" || fixture.tokenForm.Get("code_verifier") != "verifier" || fixture.tokenForm.Get("redirect_uri") != "https://app.example/auth/callback/" || fixture.tokenForm.Get("grant_type") != "authorization_code" {
		t.Errorf("token form = %v", fixture.tokenForm)
	}
}

func TestNewValidationAndDiscoveryMismatch(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("empty config was accepted")
	}

	fixture := newProviderFixture(t)
	client := fixtureClient(t, fixture)
	client.cfg.Issuer = "https://other.example"

	if _, err := client.Discover(t.Context()); err == nil {
		t.Error("mismatched discovery issuer was accepted")
	}
}

func TestVerifyIDTokenAndUserinfo(t *testing.T) {
	fixture := newProviderFixture(t)
	client := fixtureClient(t, fixture)
	tokens, err := client.Exchange(t.Context(), "code", "https://app.example/auth/callback/", "verifier")
	if err != nil {
		t.Fatal(err)
	}

	identity, err := client.VerifyIDToken(t.Context(), tokens.IDToken, "nonce")
	if err != nil {
		t.Fatal(err)
	}

	if identity.Subject != "alice" || identity.Name != "Alice Example" || identity.Username != "alice" {
		t.Errorf("identity = %+v", identity)
	}

	userinfo, err := client.Userinfo(t.Context(), tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	if err := identity.Merge(userinfo); err != nil {
		t.Fatal(err)
	}

	if _, err := client.VerifyIDToken(t.Context(), tokens.IDToken, "other"); err == nil {
		t.Error("wrong nonce was accepted")
	}

	if _, err := client.VerifyIDToken(t.Context(), signedTestToken(t, fixture.key, fixture.kid, claims{
		Issuer: fixture.server.URL,
		Audience: "other-client",
		Expires: time.Now().Add(time.Hour).Unix(),
		Issued: time.Now().Unix(),
		Nonce: "nonce",
		Subject: "alice",
	}), "nonce"); err == nil {
		t.Error("wrong audience was accepted")
	}

	if _, err := client.VerifyIDToken(t.Context(), signedTestToken(t, fixture.key, fixture.kid, claims{
		Issuer: fixture.server.URL,
		Audience: fixture.clientID,
		Expires: time.Now().Add(-time.Hour).Unix(),
		Issued: time.Now().Unix(),
		Nonce: "nonce",
		Subject: "alice",
	}), "nonce"); err == nil {
		t.Error("expired token was accepted")
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.VerifyIDToken(t.Context(), signedTestToken(t, otherKey, fixture.kid, claims{
		Issuer: fixture.server.URL,
		Audience: fixture.clientID,
		Expires: time.Now().Add(time.Hour).Unix(),
		Issued: time.Now().Unix(),
		Nonce: "nonce",
		Subject: "alice",
	}), "nonce"); err == nil {
		t.Error("token with a bad signature was accepted")
	}

	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setSigningMaterial(rotatedKey, "rotated-key")

	rotated, err := client.Exchange(t.Context(), "code", "https://app.example/auth/callback/", "verifier")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.VerifyIDToken(t.Context(), rotated.IDToken, "nonce"); err != nil {
		t.Errorf("rotated signing key was not refreshed: %v", err)
	}
}
