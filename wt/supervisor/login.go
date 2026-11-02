package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	authCookieMaxAge = 365 * 24 * 60 * 60
	loginMaxFailures = 5
	loginWindow      = time.Minute
	loginLockout     = time.Minute
)

type loginRequest struct {
	Token string `json:"token"`
}

type loginAttempt struct {
	failures     int
	firstFailure time.Time
	blockedUntil time.Time
}

// loginLimiter rate-limits failed token attempts by remote host. The supervisor is normally single-operator, so an in-memory window is enough and resets on restart.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string]loginAttempt{}}
}

// allow reports whether another attempt is permitted and how long to wait when it is not.
func (l *loginLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	attempt := l.attempts[key]
	if attempt.blockedUntil.After(now) {
		return false, attempt.blockedUntil.Sub(now)
	}

	if !attempt.firstFailure.IsZero() && now.Sub(attempt.firstFailure) > loginWindow {
		delete(l.attempts, key)
	}

	return true, 0
}

// fail records one failed attempt and starts a lockout at the threshold.
func (l *loginLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	attempt := l.attempts[key]
	if attempt.firstFailure.IsZero() || now.Sub(attempt.firstFailure) > loginWindow {
		attempt = loginAttempt{firstFailure: now}
	}

	attempt.failures++
	if attempt.failures >= loginMaxFailures {
		attempt.blockedUntil = now.Add(loginLockout)
	}

	l.attempts[key] = attempt
}

// reset clears an address's failure window after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.attempts, key)
}

// handleLogin checks the token or scoped credential and issues the admin cookie. The panel still sends a bearer token for its own requests, but EventSource can only use the cookie.
func (s *supervisor) handleLogin(w http.ResponseWriter, r *http.Request) {
	limiter := s.loginGuard("operator")
	key := s.loginKey(r)

	now := time.Now()
	if allowed, wait := limiter.allow(key, now); !allowed {
		writeRetryAfter(w, wait)
		s.appendAudit(authIdentity{Name: "anonymous"}, r, "login.failed", "", http.StatusTooManyRequests, "too many attempts")
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")

		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxConfigRequestBytes)

	var req loginRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		limiter.fail(key, now)
		s.appendAudit(authIdentity{Name: "anonymous"}, r, "login.failed", "", http.StatusBadRequest, err.Error())
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())

		return
	}

	token := strings.TrimSpace(req.Token)
	identity := anonymousAdmin()

	if s.authRequired() {
		var ok bool

		identity, ok = s.identityForToken(token)
		if !ok {
			limiter.fail(key, now)
			s.appendAudit(authIdentity{Name: "anonymous"}, r, "login.failed", "", http.StatusUnauthorized, "invalid token")
			writeError(w, http.StatusUnauthorized, "invalid token")

			return
		}
	} else {
		token = ""
	}

	limiter.reset(key)
	w.Header().Set("Cache-Control", "no-store")
	s.setAuthCookie(w, r, token, authCookieMaxAge)
	s.appendAudit(identity, r, "login", "", http.StatusNoContent, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout clears the panel cookie.
func (s *supervisor) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.setAuthCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
