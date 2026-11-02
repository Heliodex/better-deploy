// Platform identity HTTP: app-host login/logout/me/password and admin user management.

package main

import (
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"
)

const identityAppPrefix = "/_wt/identity"

type identityMeResponse struct {
	User      identityUserInfo `json:"user"`
	CSRFToken string           `json:"csrfToken"`
}

type identityLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type identityPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type identityCreateUserRequest struct {
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Password string   `json:"password"`
	Groups   []string `json:"groups"`
}

type identityUpdateUserRequest struct {
	Name     *string   `json:"name"`
	Groups   *[]string `json:"groups"`
	Disabled *bool     `json:"disabled"`
}

type identityResetPasswordRequest struct {
	Password string `json:"password"`
}

type identityUsersResponse struct {
	Users []identityUserInfo `json:"users"`
}

// identityAppRoutes returns the cached table for the per-app identity endpoints.
func (s *supervisor) identityAppRoutes() *routeTable {
	return s.identityRoutesCache.get(func() *routeTable {
		rt := newRouteTable()
		rt.add(http.MethodPost, identityAppPrefix+"/login", plainHandler(s.handleIdentityLogin))
		rt.add(http.MethodPost, identityAppPrefix+"/logout", plainHandler(s.handleIdentityLogout))
		rt.add(http.MethodGet, identityAppPrefix+"/me", plainHandler(s.handleIdentityMe))
		rt.add(http.MethodPost, identityAppPrefix+"/password", plainHandler(s.handleIdentityPassword))

		return rt
	})
}

// serveIdentityApp serves an identity route and reports whether the path belonged to one.
func (s *supervisor) serveIdentityApp(w http.ResponseWriter, r *http.Request) bool {
	routes := s.identityAppRoutes()
	if !routes.matches(r.URL.Path) {
		return false
	}

	if s.identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity is unavailable")
		return true
	}

	routes.ServeHTTP(w, r)

	return true
}

// writeIdentityError maps identity errors to HTTP statuses.
func writeIdentityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errIdentityNoSession):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, errIdentityCSRF):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, errIdentityNoUser), errors.Is(err, errIdentityDisabled):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, errIdentityUserExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errIdentityUserMissing):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errIdentityEmail), errors.Is(err, errIdentityName), errors.Is(err, errIdentityGroups), errors.Is(err, errIdentityPassword):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// handleIdentityLogin applies the login limiter, authenticates the user and starts a session.
func (s *supervisor) handleIdentityLogin(w http.ResponseWriter, r *http.Request) {
	limiter := s.loginGuard("identity")
	key := s.loginKey(r)

	now := time.Now()
	if allowed, wait := limiter.allow(key, now); !allowed {
		writeRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")

		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)

	var request identityLoginRequest
	if err := json.UnmarshalRead(r.Body, &request); err != nil {
		limiter.fail(key, now)
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())

		return
	}

	user, err := s.identity.authenticate(request.Email, request.Password)
	if err != nil {
		limiter.fail(key, now)
		writeIdentityError(w, err)

		return
	}

	token, session, err := s.identity.newSession(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	limiter.reset(key)
	s.setIdentityCookie(w, r, token)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, identityMeResponse{User: user, CSRFToken: session.CSRF})
}

// handleIdentityLogout deletes the session and clears the cookie.
func (s *supervisor) handleIdentityLogout(w http.ResponseWriter, r *http.Request) {
	session, _, token, ok := s.identityRequest(r)
	if ok {
		if !identityCSRFMatches(r, session) {
			writeIdentityError(w, errIdentityCSRF)
			return
		}

		if err := s.identity.deleteSession(token); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	s.clearIdentityCookie(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleIdentityMe returns the current user and CSRF token.
func (s *supervisor) handleIdentityMe(w http.ResponseWriter, r *http.Request) {
	session, user, _, ok := s.identityRequest(r)
	if !ok {
		writeIdentityError(w, errIdentityNoSession)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, identityMeResponse{User: user, CSRFToken: session.CSRF})
}

// handleIdentityPassword verifies the current password, changes it and starts a fresh session.
func (s *supervisor) handleIdentityPassword(w http.ResponseWriter, r *http.Request) {
	session, user, _, ok := s.identityRequest(r)
	if !ok {
		writeIdentityError(w, errIdentityNoSession)
		return
	}

	if !identityCSRFMatches(r, session) {
		writeIdentityError(w, errIdentityCSRF)
		return
	}

	var request identityPasswordRequest
	if !readJSONBody(w, r, 16<<10, &request) {
		return
	}

	if _, err := s.identity.authenticate(user.Email, request.CurrentPassword); err != nil {
		writeIdentityError(w, errIdentityNoUser)
		return
	}

	if err := s.identity.setPassword(user.ID, request.NewPassword); err != nil {
		writeIdentityError(w, err)
		return
	}

	newToken, newSession, err := s.identity.newSession(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.setIdentityCookie(w, r, newToken)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, identityMeResponse{User: user, CSRFToken: newSession.CSRF})
}

// identityCSRFMatches compares the request header with the session CSRF token in constant time.
func identityCSRFMatches(r *http.Request, session identitySession) bool {
	value := strings.TrimSpace(r.Header.Get(identityCSRFHeader))
	if value == "" || len(value) != len(session.CSRF) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(value), []byte(session.CSRF)) == 1
}

func (s *supervisor) identityAdmin(w http.ResponseWriter, r *http.Request) bool {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return false
	}

	if !identity.isAdmin() {
		writeError(w, http.StatusForbidden, "admin token required")
		return false
	}

	return true
}

// handleIdentityUsers lists or creates platform users.
func (s *supervisor) handleIdentityUsers(w http.ResponseWriter, r *http.Request) {
	if !s.identityAdmin(w, r) {
		return
	}

	if s.identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity is unavailable")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, identityUsersResponse{Users: s.identity.listUsers()})
	case http.MethodPost:
		var request identityCreateUserRequest
		if !readJSONBody(w, r, 16<<10, &request) {
			return
		}

		user, err := s.identity.createUser(request.Email, request.Name, request.Password, request.Groups)
		if err != nil {
			writeIdentityError(w, err)
			return
		}

		writeJSON(w, http.StatusCreated, user)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleIdentityUserPassword resets one user's password.
func (s *supervisor) handleIdentityUserPassword(w http.ResponseWriter, r *http.Request, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var request identityResetPasswordRequest
	if !readJSONBody(w, r, 16<<10, &request) {
		return
	}

	if err := s.identity.setPassword(id, request.Password); err != nil {
		writeIdentityError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleIdentityUserGet serves one user.
func (s *supervisor) handleIdentityUserGet(w http.ResponseWriter, id string) {
	user, ok := s.identity.user(id)
	if !ok {
		writeIdentityError(w, errIdentityUserMissing)
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// handleIdentityUserUpdate applies a name, group or disabled change.
func (s *supervisor) handleIdentityUserUpdate(w http.ResponseWriter, r *http.Request, id string) {
	var request identityUpdateUserRequest
	if !readJSONBody(w, r, 16<<10, &request) {
		return
	}

	current, ok := s.identity.user(id)
	if !ok {
		writeIdentityError(w, errIdentityUserMissing)
		return
	}

	name := current.Name
	if request.Name != nil {
		name = *request.Name
	}

	groups := current.Groups
	if request.Groups != nil {
		groups = *request.Groups
	}

	user, err := s.identity.updateUser(id, name, groups, request.Disabled)
	if err != nil {
		writeIdentityError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// handleIdentityUserDelete removes one user and their sessions.
func (s *supervisor) handleIdentityUserDelete(w http.ResponseWriter, id string) {
	if err := s.identity.deleteUser(id); err != nil {
		writeIdentityError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleIdentityUser dispatches get, update, delete and password requests for one user.
func (s *supervisor) handleIdentityUser(w http.ResponseWriter, r *http.Request, rest string) {
	if !s.identityAdmin(w, r) {
		return
	}

	if s.identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity is unavailable")
		return
	}

	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, http.StatusNotFound, errIdentityUserMissing.Error())
		return
	}

	if action == "password" {
		s.handleIdentityUserPassword(w, r, id)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleIdentityUserGet(w, id)
	case http.MethodPatch:
		s.handleIdentityUserUpdate(w, r, id)
	case http.MethodDelete:
		s.handleIdentityUserDelete(w, id)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
