package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	oauthRequestTimeout = 10 * time.Second
	metadataTTL         = 10 * time.Minute
)

// oauthConfig is the provider side of the login flow. The defaults point at the public Duende demo, which accepts any redirect URI for its interactive.public client.
type oauthConfig struct {
	issuer, providerName, clientID, clientSecret, baseURL string
	scopes                                                []string
}

// providerMetadata is the subset of the discovery document the app uses.
type providerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	JWKSUri               string `json:"jwks_uri"`
}

// tokenResponse is the authorization server's code exchange reply.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// jwk is the RSA key shape the demo's JWKS document returns.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// oauthClaims is the claim set shared by the ID token and the UserInfo response.
type oauthClaims struct {
	Issuer            string `json:"iss"`
	Audience          any    `json:"aud"`
	Expires           int64  `json:"exp"`
	Issued            int64  `json:"iat"`
	Nonce             string `json:"nonce"`
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
	Zoneinfo          string `json:"zoneinfo"`
}

// merge fills gaps in the ID token claims from the UserInfo response.
func (c *oauthClaims) merge(other oauthClaims) {
	if other.Sub != "" {
		c.Sub = other.Sub
	}

	for target, value := range map[*string]string{
		&c.Name:              other.Name,
		&c.Email:             other.Email,
		&c.PreferredUsername: other.PreferredUsername,
		&c.Picture:           other.Picture,
		&c.Zoneinfo:          other.Zoneinfo,
	} {
		if value != "" {
			*target = value
		}
	}
}

// profile turns claims into the fields a session keeps.
func (c *oauthClaims) profile() profile {
	username := strings.TrimSpace(c.PreferredUsername)
	if username == "" {
		username = strings.TrimSpace(c.Email)
		if local, _, ok := strings.Cut(username, "@"); ok {
			username = local
		}
	}

	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = username
	}

	return profile{
		Sub:      strings.TrimSpace(c.Sub),
		Name:     name,
		Email:    strings.TrimSpace(c.Email),
		Username: username,
		Picture:  strings.TrimSpace(c.Picture),
		Zoneinfo: strings.TrimSpace(c.Zoneinfo),
	}
}

// oauthClient caches the discovery document and signing keys between requests to the same instance.
type oauthClient struct {
	cfg        oauthConfig
	httpClient *http.Client

	mu             sync.Mutex
	metadata       providerMetadata
	metadataExpiry time.Time

	keysMu     sync.Mutex
	keys       map[string]*rsa.PublicKey
	keysExpiry time.Time
}

// newOAuthClient reads the provider settings from the environment.
func newOAuthClient(httpClient *http.Client) *oauthClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &oauthClient{
		cfg: oauthConfig{
			baseURL:      strings.TrimRight(envDefault("OAUTH_BASE_URL", ""), "/"),
			clientID:     envDefault("OAUTH_CLIENT_ID", "interactive.public"),
			clientSecret: envDefault("OAUTH_CLIENT_SECRET", ""),
			issuer:       strings.TrimRight(envDefault("OAUTH_ISSUER", "https://demo.duendesoftware.com"), "/"),
			providerName: envDefault("OAUTH_PROVIDER_NAME", "Duende IdentityServer demo"),
			scopes:       scopesFromEnv(),
		},
		httpClient: httpClient,
	}
}

// hostname strips any port from a Host or authority.
func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}

	return host
}

// loopbackHost reports whether a host is a local development name.
func loopbackHost(host string) bool {
	name := strings.Trim(strings.ToLower(hostname(host)), "[]")

	return name == "localhost" || strings.HasSuffix(name, ".localhost") || name == "127.0.0.1" || name == "::1"
}

// baseURL is the browser-facing origin used as the OAuth redirect URI. In wt dev, Vite keeps the browser on 127.0.0.1 and rewrites Host to <app>.localhost, so a loopback Referer supplies the port the browser actually uses.
func (o *oauthClient) baseURL(r *http.Request) string {
	if o.cfg.baseURL != "" {
		return o.cfg.baseURL
	}

	scheme := "http"
	if r.TLS != nil || r.URL.Scheme == "https" || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}

	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	// A same-origin form POST or fetch carries the browser's real origin, including the scheme and port. Use it when the host matches, so the redirect URI is correct behind an ingress that terminates TLS.
	if origin := r.Header.Get("Origin"); origin != "" {
		if parsed, err := url.Parse(origin); err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			if strings.EqualFold(parsed.Host, host) || (loopbackHost(host) && loopbackHost(parsed.Host)) {
				return parsed.Scheme + "://" + parsed.Host
			}
		}
	}

	if referer := r.Referer(); referer != "" {
		if parsed, err := url.Parse(referer); err == nil && parsed.Host != "" && loopbackHost(host) && loopbackHost(parsed.Host) {
			if parsed.Scheme != "" {
				scheme = parsed.Scheme
			}

			host = parsed.Host
		}
	}

	return scheme + "://" + host
}

// request runs one OAuth back-channel request with a short deadline.
func (o *oauthClient) request(ctx context.Context, method, target string, body io.Reader, contentType string) (*http.Response, error) {
	return o.requestAuth(ctx, method, target, body, contentType, "", "")
}

// requestAuth is request with optional HTTP Basic client authentication.
func (o *oauthClient) requestAuth(ctx context.Context, method, target string, body io.Reader, contentType, username, password string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, oauthRequestTimeout)

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		cancel()
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wt-passport-example/2")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}

	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// cancelOnClose releases the request deadline once the body is closed.
type cancelOnClose struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()

	return err
}

// decodeJSON reads a bounded JSON body and closes it.
func decodeJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", resp.Request.URL, err)
	}

	return nil
}

// oauthError is the standard error body an authorization server returns.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// errorBody reads an OAuth error response, falling back to the raw text.
func errorBody(resp *http.Response) error {
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var body oauthError
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return fmt.Errorf("%s: %s", body.Error, body.ErrorDescription)
	}

	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
}

// discover fetches and caches the OpenID Connect discovery document.
func (o *oauthClient) discover(ctx context.Context) (providerMetadata, error) {
	o.mu.Lock()
	if o.metadata.AuthorizationEndpoint != "" && time.Now().Before(o.metadataExpiry) {
		metadata := o.metadata
		o.mu.Unlock()

		return metadata, nil
	}
	o.mu.Unlock()

	resp, err := o.request(ctx, http.MethodGet, o.cfg.issuer+"/.well-known/openid-configuration", nil, "")
	if err != nil {
		return providerMetadata{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return providerMetadata{}, fmt.Errorf("discovery: %w", errorBody(resp))
	}

	var metadata providerMetadata
	if err := decodeJSON(resp, &metadata); err != nil {
		return providerMetadata{}, err
	}

	if strings.TrimRight(metadata.Issuer, "/") != o.cfg.issuer {
		return providerMetadata{}, fmt.Errorf("discovery issuer %q does not match OAUTH_ISSUER", metadata.Issuer)
	}

	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" {
		return providerMetadata{}, errors.New("discovery document is missing an authorization or token endpoint")
	}

	o.mu.Lock()
	o.metadata = metadata
	o.metadataExpiry = time.Now().Add(metadataTTL)
	o.mu.Unlock()

	return metadata, nil
}

// cachedMetadata returns an unexpired discovery document without making a network request.
func (o *oauthClient) cachedMetadata() (providerMetadata, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.metadata.AuthorizationEndpoint == "" || time.Now().After(o.metadataExpiry) {
		return providerMetadata{}, false
	}

	return o.metadata, true
}

// newPKCE returns a verifier and its S256 challenge.
func newPKCE() (verifier, challenge string, err error) {
	verifier, err = newToken()
	if err != nil {
		return "", "", err
	}

	sum := sha256.Sum256([]byte(verifier))

	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// exchange redeems an authorization code for tokens.
func (o *oauthClient) exchange(ctx context.Context, metadata providerMetadata, code string, flow callbackFlow) (tokenResponse, error) {
	form := url.Values{
		"client_id":     {o.cfg.clientID},
		"code":          {code},
		"code_verifier": {flow.verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {flow.redirectURI},
	}

	username, password := "", ""
	if o.cfg.clientSecret != "" {
		username, password = o.cfg.clientID, o.cfg.clientSecret
	}

	resp, err := o.requestAuth(ctx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", username, password)
	if err != nil {
		return tokenResponse{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("token endpoint: %w", errorBody(resp))
	}

	var tokens tokenResponse
	if err := decodeJSON(resp, &tokens); err != nil {
		return tokenResponse{}, err
	}

	if tokens.Error != "" {
		return tokenResponse{}, fmt.Errorf("%s: %s", tokens.Error, tokens.ErrorDescription)
	}

	if tokens.AccessToken == "" || tokens.IDToken == "" {
		return tokenResponse{}, errors.New("token response is missing an access token or ID token")
	}

	return tokens, nil
}

// decodeSegment decodes a base64url JWT segment with or without padding.
func decodeSegment(segment string) ([]byte, error) {
	if data, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return data, nil
	}

	data, err := base64.URLEncoding.DecodeString(segment)
	if err != nil {
		return nil, fmt.Errorf("decode base64url: %w", err)
	}

	return data, nil
}

// fetchKeys downloads the provider's JWKS and turns its RSA keys into public keys.
func (o *oauthClient) fetchKeys(ctx context.Context, metadata providerMetadata) (map[string]*rsa.PublicKey, error) {
	resp, err := o.request(ctx, http.MethodGet, metadata.JWKSUri, nil, "")
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: %w", errorBody(resp))
	}

	var document jwksDocument
	if err := decodeJSON(resp, &document); err != nil {
		return nil, err
	}

	keys := make(map[string]*rsa.PublicKey, len(document.Keys))

	for _, key := range document.Keys {
		if key.Kty != "RSA" || key.N == "" || key.E == "" {
			continue
		}

		modulus, err := decodeSegment(key.N)
		if err != nil {
			continue
		}

		exponent, err := decodeSegment(key.E)
		if err != nil {
			continue
		}

		exponentInt := new(big.Int).SetBytes(exponent)
		if !exponentInt.IsInt64() || exponentInt.Int64() > int64(^uint(0)>>1) {
			continue
		}

		keys[key.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponentInt.Int64())}
	}

	if len(keys) == 0 {
		return nil, errors.New("jwks has no usable RSA keys")
	}

	return keys, nil
}

// signingKey returns the key for a JWT header, refreshing the cache when the key is new.
func (o *oauthClient) signingKey(ctx context.Context, metadata providerMetadata, kid string) (*rsa.PublicKey, error) {
	o.keysMu.Lock()
	if key := o.keys[kid]; key != nil && time.Now().Before(o.keysExpiry) {
		o.keysMu.Unlock()
		return key, nil
	}
	o.keysMu.Unlock()

	keys, err := o.fetchKeys(ctx, metadata)
	if err != nil {
		return nil, err
	}

	o.keysMu.Lock()
	o.keys = keys
	o.keysExpiry = time.Now().Add(metadataTTL)
	o.keysMu.Unlock()

	if key := keys[kid]; key != nil {
		return key, nil
	}

	return nil, fmt.Errorf("jwks has no key %q", kid)
}

// audienceContains reports whether an aud claim names the client.
func audienceContains(audience any, clientID string) bool {
	switch value := audience.(type) {
	case string:
		return value == clientID
	case []any:
		for _, item := range value {
			if name, ok := item.(string); ok && name == clientID {
				return true
			}
		}
	}

	return false
}

// verifyIDToken checks the RS256 signature and the issuer, audience, expiry and nonce claims.
func (o *oauthClient) verifyIDToken(ctx context.Context, metadata providerMetadata, raw, expectedNonce string) (oauthClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return oauthClaims{}, errors.New("id token is not a compact JWT")
	}

	headerBytes, err := decodeSegment(parts[0])
	if err != nil {
		return oauthClaims{}, fmt.Errorf("decode id token header: %w", err)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return oauthClaims{}, fmt.Errorf("parse id token header: %w", err)
	}

	if header.Alg != "RS256" {
		return oauthClaims{}, fmt.Errorf("unsupported id token algorithm %q", header.Alg)
	}

	key, err := o.signingKey(ctx, metadata, header.Kid)
	if err != nil {
		return oauthClaims{}, err
	}

	signature, err := decodeSegment(parts[2])
	if err != nil {
		return oauthClaims{}, fmt.Errorf("decode id token signature: %w", err)
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return oauthClaims{}, fmt.Errorf("id token signature: %w", err)
	}

	payloadBytes, err := decodeSegment(parts[1])
	if err != nil {
		return oauthClaims{}, fmt.Errorf("decode id token claims: %w", err)
	}

	var claims oauthClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return oauthClaims{}, fmt.Errorf("parse id token claims: %w", err)
	}

	now := time.Now().Unix()

	if strings.TrimRight(claims.Issuer, "/") != o.cfg.issuer {
		return oauthClaims{}, fmt.Errorf("id token issuer %q is not %q", claims.Issuer, o.cfg.issuer)
	}

	if !audienceContains(claims.Audience, o.cfg.clientID) {
		return oauthClaims{}, errors.New("id token audience does not include the client")
	}

	if claims.Expires == 0 || now > claims.Expires+60 {
		return oauthClaims{}, errors.New("id token has expired")
	}

	if claims.Issued != 0 && claims.Issued > now+120 {
		return oauthClaims{}, errors.New("id token was issued in the future")
	}

	if expectedNonce == "" || claims.Nonce != expectedNonce {
		return oauthClaims{}, errors.New("id token nonce does not match the login flow")
	}

	return claims, nil
}

// userinfo calls the protected UserInfo endpoint with the access token.
func (o *oauthClient) userinfo(ctx context.Context, metadata providerMetadata, accessToken string) (oauthClaims, error) {
	if metadata.UserinfoEndpoint == "" {
		return oauthClaims{}, errors.New("provider has no userinfo endpoint")
	}

	ctx, cancel := context.WithTimeout(ctx, oauthRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata.UserinfoEndpoint, nil)
	if err != nil {
		return oauthClaims{}, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "wt-passport-example/2")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return oauthClaims{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return oauthClaims{}, fmt.Errorf("userinfo: %w", errorBody(resp))
	}

	var claims oauthClaims
	if err := decodeJSON(resp, &claims); err != nil {
		return oauthClaims{}, err
	}

	return claims, nil
}
