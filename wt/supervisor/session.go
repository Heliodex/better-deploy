package main

import (
	"net/http"
	"strconv"
	"time"
)

const authCookieName = "wt_token"

// setSessionCookie writes an HttpOnly, SameSite=Lax session cookie with the forwarded scheme.
func (s *supervisor) setSessionCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	secure := s.forwardedInfo(r).Proto == "https"
	//nolint:gosec // Secure follows the forwarded request scheme.
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// setAuthCookie writes the operator token as an HttpOnly cookie so event streams authenticate without JavaScript access.
func (s *supervisor) setAuthCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	s.setSessionCookie(w, r, authCookieName, token, maxAge)
}

// loginGuard returns the rate limiter for one credential plane.
func (s *supervisor) loginGuard(scope string) *loginLimiter {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	if s.loginLimiters == nil {
		s.loginLimiters = map[string]*loginLimiter{}
	}

	limiter := s.loginLimiters[scope]
	if limiter == nil {
		limiter = newLoginLimiter()
		s.loginLimiters[scope] = limiter
	}

	return limiter
}

// loginKey returns the remote host used for rate limiting, preferring the trusted forwarded client.
func (s *supervisor) loginKey(r *http.Request) string {
	key := s.forwardedInfo(r).ClientIP
	if key == "" {
		return remoteHost(r)
	}

	return key
}

// writeRetryAfter reports how long a rate-limited caller should wait.
func writeRetryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(int(wait.Seconds()), 1)))
}
