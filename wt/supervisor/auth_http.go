package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type authTokenCreateRequest struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Apps      []string `json:"apps"`
	ExpiresIn string   `json:"expires_in"`
}

type authTokenCreateResponse struct {
	authTokenInfo

	Token string `json:"token"`
}

type authTokensResponse struct {
	Tokens []authTokenInfo `json:"tokens"`
}

// authRequired reports whether admin and API requests must carry a token. Untrusted tenancy and any stored scoped token always require one.
func (s *supervisor) authRequired() bool {
	return s.cfg.token != "" || s.cfg.untrusted() || (s.auth != nil && s.auth.count() > 0)
}

// anonymousAdmin keeps the loopback-only, tokenless trusted mode working.
func anonymousAdmin() authIdentity {
	return authIdentity{Name: "anonymous", Role: roleAdmin, Apps: []string{"*"}}
}

// identityForToken resolves the bootstrap operator token or a stored scoped token.
func (s *supervisor) identityForToken(raw string) (authIdentity, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return authIdentity{}, false
	}

	if s.cfg.token != "" && secureEqual(raw, s.cfg.token) {
		return authIdentity{Name: "operator", Role: roleAdmin, Apps: []string{"*"}}, true
	}

	if s.auth != nil {
		if identity, ok := s.auth.identity(raw); ok {
			return identity, true
		}
	}

	return authIdentity{}, false
}

// requestIdentity authenticates a bearer header or the panel cookie as the bootstrap operator or a stored scoped token.
func (s *supervisor) requestIdentity(r *http.Request) (authIdentity, bool) {
	if !s.authRequired() {
		return anonymousAdmin(), true
	}

	var raw string
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		raw = strings.TrimSpace(token)
	} else if cookie, err := r.Cookie("wt_token"); err == nil {
		raw = strings.TrimSpace(cookie.Value)
	}

	return s.identityForToken(raw)
}

func (s *supervisor) authorised(r *http.Request) bool {
	_, ok := s.requestIdentity(r)
	return ok
}

// requireIdentity writes 401 unless the request carries a valid token.
func (s *supervisor) requireIdentity(w http.ResponseWriter, r *http.Request) (authIdentity, bool) {
	identity, ok := s.requestIdentity(r)
	if ok {
		return identity, true
	}

	w.Header().Set("WWW-Authenticate", `Bearer realm="wt-supervisor"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)

	return authIdentity{}, false
}

func (s *supervisor) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	_, ok := s.requireIdentity(w, r)
	return ok
}

// requireAdmin is for token management, the audit log and other host-wide writes.
func (s *supervisor) requireAdmin(w http.ResponseWriter, r *http.Request) (authIdentity, bool) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return authIdentity{}, false
	}

	if !identity.isAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return authIdentity{}, false
	}

	return identity, true
}

// requireApp authenticates the request and checks that the identity may see this app.
func (s *supervisor) requireApp(w http.ResponseWriter, r *http.Request, name string) (*app, bool) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return nil, false
	}

	if !identity.canAccessApp(name) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}

	a := s.lookup(name)
	if a == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return nil, false
	}

	return a, true
}

// handleAuthTokens lists or creates scoped tokens. The bearer value is returned only by the create call.
func (s *supervisor) handleAuthTokens(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth store unavailable")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, authTokensResponse{Tokens: s.auth.list()})
	case http.MethodPost:
		var req authTokenCreateRequest
		if !readJSONBody(w, r, maxConfigRequestBytes, &req) {
			return
		}

		expiresAt, err := parseTokenExpiry(req.ExpiresIn)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		token, info, err := s.auth.create(req.Name, req.Role, req.Apps, expiresAt)
		if err != nil {
			s.appendAudit(identity, r, "token.create", "", http.StatusBadRequest, err.Error())
			writeError(w, http.StatusBadRequest, err.Error())

			return
		}

		s.appendAudit(identity, r, "token.create", "", http.StatusCreated, "")
		writeJSON(w, http.StatusCreated, authTokenCreateResponse{Token: token, authTokenInfo: info})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// parseTokenExpiry accepts a bounded duration, or empty and "never" for no expiry.
func parseTokenExpiry(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" || strings.EqualFold(value, "never") {
		return time.Time{}, nil
	}

	expires, err := time.ParseDuration(value)
	if err != nil || expires < minTokenExpiry || expires > maxTokenExpiry {
		return time.Time{}, fmt.Errorf("expires_in %q must be between %s and %s, or empty for no expiry", value, minTokenExpiry, maxTokenExpiry)
	}

	return time.Now().UTC().Add(expires), nil
}

// handleAuthToken deletes one scoped token.
func (s *supervisor) handleAuthToken(w http.ResponseWriter, r *http.Request, id string) {
	identity, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	if !requireMethod(w, r, http.MethodDelete) {
		return
	}

	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth store unavailable")
		return
	}

	if err := s.auth.remove(id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errAuthTokenNotFound) {
			status = http.StatusNotFound
			http.Error(w, "token not found", status)
			s.appendAudit(identity, r, "token.delete", "", status, err.Error())

			return
		}

		s.appendAudit(identity, r, "token.delete", "", status, err.Error())
		writeError(w, status, err.Error())

		return
	}

	s.appendAudit(identity, r, "token.delete", "", http.StatusNoContent, "")
	w.WriteHeader(http.StatusNoContent)
}
