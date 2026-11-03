// Package oauth implements the OAuth 2.0 authorisation code flow with PKCE and OpenID Connect discovery, token verification and UserInfo.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultRequestTimeout = 10 * time.Second
	metadataTTL           = 10 * time.Minute
	clockSkew             = 60 * time.Second
)

// Config describes one OAuth client. HTTPClient must be a wasi:http client in a component; tests can pass a normal client.
type Config struct {
	Issuer, ClientID, ClientSecret string
	Scopes                         []string
	HTTPClient                     *http.Client
	Timeout                        time.Duration
}

// Provider is the subset of the discovery document the client uses.
type Provider struct {
	Issuer                string `json:"issuer"`
	AuthorisationEndpoint string `json:"authorisation_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	JWKSUri               string `json:"jwks_uri"`
}

// Tokens is the token endpoint response.
type Tokens struct {
	AccessToken, RefreshToken, IDToken, TokenType, Scope string
	ExpiresIn                                            int64
	Error, ErrorDescription                              string
}

// Identity is the user information from an ID token or UserInfo response.
type Identity struct {
	Subject, Name, Email, Username, Picture, Zoneinfo, Issuer string
	Audience                                                  []string
	ExpiresAt, IssuedAt                                       time.Time
}

// Merge fills missing profile fields from a UserInfo response. A different subject is an error.
func (i *Identity) Merge(other Identity) error {
	if i.Subject != "" && other.Subject != "" && i.Subject != other.Subject {
		return errors.New("userinfo subject does not match the ID token subject")
	}

	if i.Subject == "" {
		i.Subject = other.Subject
	}

	for target, value := range map[*string]string{
		&i.Name:     other.Name,
		&i.Email:    other.Email,
		&i.Username: other.Username,
		&i.Picture:  other.Picture,
		&i.Zoneinfo: other.Zoneinfo,
	} {
		if *target == "" {
			*target = value
		}
	}

	if i.Issuer == "" {
		i.Issuer = other.Issuer
	}

	if len(i.Audience) == 0 && len(other.Audience) > 0 {
		i.Audience = append([]string(nil), other.Audience...)
	}

	if i.ExpiresAt.IsZero() {
		i.ExpiresAt = other.ExpiresAt
	}

	if i.IssuedAt.IsZero() {
		i.IssuedAt = other.IssuedAt
	}

	return nil
}

// claims is the common claim set from an ID token or UserInfo response.
type claims struct {
	Issuer            string `json:"iss"`
	Audience          any    `json:"aud"`
	Expires           int64  `json:"exp"`
	Issued            int64  `json:"iat"`
	Nonce             string `json:"nonce"`
	Subject           string `json:"sub"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
	Zoneinfo          string `json:"zoneinfo"`
}

// identity converts claims into the shared profile shape and fills the username from the email when needed.
func (c claims) identity() Identity {
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

	identity := Identity{
		Subject:   strings.TrimSpace(c.Subject),
		Name:      name,
		Email:     strings.TrimSpace(c.Email),
		Username:  username,
		Picture:   strings.TrimSpace(c.Picture),
		Zoneinfo:  strings.TrimSpace(c.Zoneinfo),
		Issuer:    strings.TrimSpace(c.Issuer),
		Audience:  audienceStrings(c.Audience),
		ExpiresAt: unixTime(c.Expires),
		IssuedAt:  unixTime(c.Issued),
	}

	return identity
}

// audienceStrings normalises an aud claim, which may be one string or an array.
func audienceStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				out = append(out, name)
			}
		}

		return out
	}

	return nil
}

// unixTime converts a numeric claim to a time, leaving the zero value for 0.
func unixTime(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}

	return time.Unix(seconds, 0).UTC()
}

// Client caches discovery metadata and signing keys between requests.
type Client struct {
	cfg        Config
	httpClient *http.Client

	mu             sync.Mutex
	provider       Provider
	providerExpiry time.Time

	keysMu     sync.Mutex
	keys       map[string]*rsa.PublicKey
	keysExpiry time.Time
}

// New validates a config and returns a client.
func New(cfg Config) (*Client, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)

	if cfg.Issuer == "" {
		return nil, errors.New("oauth: issuer is required")
	}

	if cfg.ClientID == "" {
		return nil, errors.New("oauth: client ID is required")
	}

	if len(cfg.Scopes) == 0 {
		cfg.Scopes = strings.Fields("openid profile email")
	}

	cfg.Scopes = append([]string(nil), cfg.Scopes...)
	slices.Sort(cfg.Scopes)
	cfg.Scopes = slices.Compact(cfg.Scopes)

	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRequestTimeout
	}

	return &Client{cfg: cfg, httpClient: cfg.HTTPClient}, nil
}

// Config returns a copy of the client configuration.
func (c *Client) Config() Config {
	cfg := c.cfg
	cfg.Scopes = append([]string(nil), c.cfg.Scopes...)

	return cfg
}

// NewPKCE returns a verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}

	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))

	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// NewNonce returns a random state or nonce value.
func NewNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// request runs one back-channel request with the configured timeout.
func (c *Client) request(ctx context.Context, method, target string, body io.Reader, contentType string) (*http.Response, error) {
	return c.requestAuth(ctx, method, target, body, contentType, "", "")
}

// requestAuth is request with optional HTTP Basic client authentication.
func (c *Client) requestAuth(ctx context.Context, method, target string, body io.Reader, contentType, username, password string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		cancel()

		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wt-oauth/1")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()

		return nil, err
	}

	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// cancelOnClose releases the request deadline when the body is closed.
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

// errorBody reads an OAuth error response, falling back to raw text.
func errorBody(resp *http.Response) error {
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var body struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return fmt.Errorf("%s: %s", body.Error, body.ErrorDescription)
	}

	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
}

// Discover fetches and caches the OpenID Connect discovery document.
func (c *Client) Discover(ctx context.Context) (Provider, error) {
	c.mu.Lock()
	if c.provider.AuthorisationEndpoint != "" && time.Now().Before(c.providerExpiry) {
		provider := c.provider
		c.mu.Unlock()

		return provider, nil
	}
	c.mu.Unlock()

	resp, err := c.request(ctx, http.MethodGet, c.cfg.Issuer+"/.well-known/openid-configuration", nil, "")
	if err != nil {
		return Provider{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return Provider{}, fmt.Errorf("discovery: %w", errorBody(resp))
	}

	var provider Provider
	if err := decodeJSON(resp, &provider); err != nil {
		return Provider{}, err
	}

	if strings.TrimRight(provider.Issuer, "/") != c.cfg.Issuer {
		return Provider{}, fmt.Errorf("discovery issuer %q does not match %q", provider.Issuer, c.cfg.Issuer)
	}

	if provider.AuthorisationEndpoint == "" || provider.TokenEndpoint == "" {
		return Provider{}, errors.New("discovery document is missing an authorization or token endpoint")
	}

	c.mu.Lock()
	c.provider = provider
	c.providerExpiry = time.Now().Add(metadataTTL)
	c.mu.Unlock()

	return provider, nil
}

// AuthoriseURL returns the authorization endpoint URL for a code flow with PKCE.
func (c *Client) AuthoriseURL(ctx context.Context, redirectURI, state, nonce, codeChallenge string) (string, error) {
	provider, err := c.Discover(ctx)
	if err != nil {
		return "", err
	}

	params := url.Values{
		"client_id":             {c.cfg.ClientID},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"nonce":                 {nonce},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(c.cfg.Scopes, " ")},
		"state":                 {state},
	}

	return provider.AuthorisationEndpoint + "?" + params.Encode(), nil
}

// Exchange redeems an authorization code for tokens.
func (c *Client) Exchange(ctx context.Context, code, redirectURI, verifier string) (Tokens, error) {
	provider, err := c.Discover(ctx)
	if err != nil {
		return Tokens{}, err
	}

	form := url.Values{
		"client_id":     {c.cfg.ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}

	username, password := "", ""
	if c.cfg.ClientSecret != "" {
		username, password = c.cfg.ClientID, c.cfg.ClientSecret
	}

	resp, err := c.requestAuth(ctx, http.MethodPost, provider.TokenEndpoint, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", username, password)
	if err != nil {
		return Tokens{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("token endpoint: %w", errorBody(resp))
	}

	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		TokenType        string `json:"token_type"`
		Scope            string `json:"scope"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := decodeJSON(resp, &raw); err != nil {
		return Tokens{}, err
	}

	if raw.Error != "" {
		return Tokens{}, fmt.Errorf("%s: %s", raw.Error, raw.ErrorDescription)
	}

	if raw.AccessToken == "" {
		return Tokens{}, errors.New("token response is missing an access token")
	}

	return Tokens{
		AccessToken:      raw.AccessToken,
		RefreshToken:     raw.RefreshToken,
		IDToken:          raw.IDToken,
		TokenType:        raw.TokenType,
		Scope:            raw.Scope,
		ExpiresIn:        raw.ExpiresIn,
		Error:            raw.Error,
		ErrorDescription: raw.ErrorDescription,
	}, nil
}

// Userinfo calls the protected UserInfo endpoint with the access token.
func (c *Client) Userinfo(ctx context.Context, accessToken string) (Identity, error) {
	provider, err := c.Discover(ctx)
	if err != nil {
		return Identity{}, err
	}

	if provider.UserinfoEndpoint == "" {
		return Identity{}, errors.New("provider has no userinfo endpoint")
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.UserinfoEndpoint, nil)
	if err != nil {
		return Identity{}, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "wt-oauth/1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Identity{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("userinfo: %w", errorBody(resp))
	}

	var claims claims
	if err := decodeJSON(resp, &claims); err != nil {
		return Identity{}, err
	}

	return claims.identity(), nil
}

// EndSessionURL returns the provider's RP-initiated logout URL.
func (c *Client) EndSessionURL(ctx context.Context, idToken, postLogoutRedirectURI string) (string, error) {
	provider, err := c.Discover(ctx)
	if err != nil {
		return "", err
	}

	if provider.EndSessionEndpoint == "" {
		return "", errors.New("provider has no end-session endpoint")
	}

	params := url.Values{"client_id": {c.cfg.ClientID}}
	if idToken != "" {
		params.Set("id_token_hint", idToken)
	}

	if postLogoutRedirectURI != "" {
		params.Set("post_logout_redirect_uri", postLogoutRedirectURI)
	}

	return provider.EndSessionEndpoint + "?" + params.Encode(), nil
}
