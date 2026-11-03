package oauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

// timeNow keeps verification testable and centralizes the one clock read.
func timeNow() time.Time {
	return time.Now()
}

// timeNowUnix is the numeric form used by JWT claims.
func timeNowUnix() int64 {
	return timeNow().Unix()
}

// jwk is the RSA key shape a JWKS document returns.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// VerifyIDToken validates an RS256 ID token and returns its identity.
func (c *Client) VerifyIDToken(ctx context.Context, raw, nonce string) (Identity, error) {
	if raw == "" {
		return Identity{}, errors.New("id token is empty")
	}

	provider, err := c.Discover(ctx)
	if err != nil {
		return Identity{}, err
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("id token is not a compact JWT")
	}

	headerBytes, err := decodeSegment(parts[0])
	if err != nil {
		return Identity{}, fmt.Errorf("decode id token header: %w", err)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Identity{}, fmt.Errorf("parse id token header: %w", err)
	}

	if header.Alg != "RS256" {
		return Identity{}, fmt.Errorf("unsupported id token algorithm %q", header.Alg)
	}

	key, err := c.signingKey(ctx, provider, header.Kid)
	if err != nil {
		return Identity{}, err
	}

	signature, err := decodeSegment(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("decode id token signature: %w", err)
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return Identity{}, fmt.Errorf("id token signature: %w", err)
	}

	payloadBytes, err := decodeSegment(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("decode id token claims: %w", err)
	}

	var claims claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Identity{}, fmt.Errorf("parse id token claims: %w", err)
	}

	now := timeNowUnix()

	if strings.TrimRight(claims.Issuer, "/") != c.cfg.Issuer {
		return Identity{}, fmt.Errorf("id token issuer %q is not %q", claims.Issuer, c.cfg.Issuer)
	}

	if !slices.Contains(audienceStrings(claims.Audience), c.cfg.ClientID) {
		return Identity{}, errors.New("id token audience does not include the client")
	}

	if claims.Expires == 0 || now > claims.Expires+int64(clockSkew.Seconds()) {
		return Identity{}, errors.New("id token has expired")
	}

	if claims.Issued != 0 && claims.Issued > now+int64(clockSkew.Seconds()) {
		return Identity{}, errors.New("id token was issued in the future")
	}

	if nonce != "" && claims.Nonce != nonce {
		return Identity{}, errors.New("id token nonce does not match the login flow")
	}

	identity := claims.identity()
	if identity.Subject == "" {
		return Identity{}, errors.New("id token has no subject")
	}

	return identity, nil
}

// fetchKeys downloads the provider's JWKS and turns its RSA keys into public keys.
func (c *Client) fetchKeys(ctx context.Context, provider Provider) (map[string]*rsa.PublicKey, error) {
	if provider.JWKSUri == "" {
		return nil, errors.New("provider has no JWKS URI")
	}

	resp, err := c.request(ctx, http.MethodGet, provider.JWKSUri, nil, "")
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
func (c *Client) signingKey(ctx context.Context, provider Provider, kid string) (*rsa.PublicKey, error) {
	c.keysMu.Lock()
	if key := c.keys[kid]; key != nil && timeNow().Before(c.keysExpiry) {
		c.keysMu.Unlock()

		return key, nil
	}
	c.keysMu.Unlock()

	keys, err := c.fetchKeys(ctx, provider)
	if err != nil {
		return nil, err
	}

	c.keysMu.Lock()
	c.keys = keys
	c.keysExpiry = timeNow().Add(metadataTTL)
	c.keysMu.Unlock()

	if key := keys[kid]; key != nil {
		return key, nil
	}

	return nil, fmt.Errorf("jwks has no key %q", kid)
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
