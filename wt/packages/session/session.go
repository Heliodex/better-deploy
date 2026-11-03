// Package session provides opaque browser sessions, hashed tokens, cookie helpers and same-origin checks.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultName is the cookie name used when a Manager has no explicit name.
const DefaultName = "session"

// Manager owns the cookie name, lifetime and path for one app session.
type Manager struct {
	Name string
	TTL  time.Duration
	Path string
}

// New returns a session manager with sane cookie defaults.
func New(name string, ttl time.Duration) Manager {
	if strings.TrimSpace(name) == "" {
		name = DefaultName
	}

	if ttl < 0 {
		ttl = 0
	}

	return Manager{Name: name, TTL: ttl, Path: "/"}
}

// Issue creates a random session token and its stored hash.
func Issue() (token, hash string, err error) {
	token, err = NewToken()
	if err != nil {
		return "", "", err
	}

	return token, Hash(token), nil
}

// NewToken returns a URL-safe token with 32 bytes of entropy.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash is the hex SHA-256 digest stored in the database instead of the cookie value.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// Equal compares two secrets without leaking their common prefix.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Set writes a session cookie with the manager's settings.
func (m Manager) Set(w http.ResponseWriter, r *http.Request, token string) {
	path := m.Path
	if path == "" {
		path = "/"
	}

	SetPath(w, r, m.Name, token, m.TTL, path)
}

// Clear expires the manager's session cookie.
func (m Manager) Clear(w http.ResponseWriter, r *http.Request) {
	path := m.Path
	if path == "" {
		path = "/"
	}

	ClearPath(w, r, m.Name, path)
}

// Token returns the raw session cookie value.
func (m Manager) Token(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(m.Name)
	if err != nil || cookie.Value == "" {
		return "", false
	}

	return cookie.Value, true
}

// Set writes one HttpOnly, SameSite=Lax cookie at the root path.
func Set(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	SetPath(w, r, name, value, ttl, "/")
}

// SetPath writes one HttpOnly, SameSite=Lax cookie at an explicit path.
//
//nolint:gosec // Secure follows the request scheme; HttpOnly and SameSite are set.
func SetPath(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration, path string) {
	if path == "" {
		path = "/"
	}

	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   Secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear expires one cookie at the root path.
func Clear(w http.ResponseWriter, r *http.Request, name string) {
	ClearPath(w, r, name, "/")
}

// ClearPath expires one cookie at an explicit path.
//
//nolint:gosec // Secure follows the request scheme; HttpOnly and SameSite are set.
func ClearPath(w http.ResponseWriter, r *http.Request, name, path string) {
	if path == "" {
		path = "/"
	}

	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   Secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// Secure reports whether the browser reached the app over HTTPS.
func Secure(r *http.Request) bool {
	if r.TLS != nil || r.URL.Scheme == "https" {
		return true
	}

	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SameOrigin rejects a cross-site state-changing request. A missing Origin is allowed for old clients and non-browser callers; SameSite=Lax still protects the cookie. Loopback origins are accepted for the Vite dev server, which rewrites Host to <app>.localhost.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}

	if strings.EqualFold(parsed.Host, r.Host) {
		return true
	}

	return loopbackHost(parsed.Host) && loopbackHost(r.Host)
}

// hostname strips a port from a Host or authority.
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
