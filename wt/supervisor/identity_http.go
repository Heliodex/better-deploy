// Platform identity HTTP: app-host login/logout/me/password and admin user management.

package main

import (
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"
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

func identityAppAction(path string) (string, bool) {
	switch path {
	case identityAppPrefix + "/login", "/v1/login":
		return "login", true
	case identityAppPrefix + "/logout", "/v1/logout":
		return "logout", true
	case identityAppPrefix + "/me", "/v1/me":
		return "me", true
	case identityAppPrefix + "/password":
		return "password", true
	}
	return "", false
}

func (s *supervisor) identityLoginGuard() *loginLimiter {
	s.identityLoginMu.Lock()
	defer s.identityLoginMu.Unlock()
	if s.identityLogin == nil {
		s.identityLogin = newLoginLimiter()
	}
	return s.identityLogin
}

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

func (s *supervisor) handleIdentityApp(w http.ResponseWriter, r *http.Request, a *app, action string) {
	if s.identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity is unavailable")
		return
	}
	switch action {
	case "login":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleIdentityLogin(w, r, a)
	case "logout":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleIdentityLogout(w, r)
	case "me":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleIdentityMe(w, r)
	case "password":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleIdentityPassword(w, r)
	}
}

func (s *supervisor) handleIdentityLogin(w http.ResponseWriter, r *http.Request, _ *app) {
	limiter := s.identityLoginGuard()
	key := s.forwardedInfo(r).ClientIP
	if key == "" {
		key = remoteHost(r)
	}
	now := time.Now()
	if allowed, wait := limiter.allow(key, now); !allowed {
		seconds := max(int(wait.Seconds()), 1)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
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

func (s *supervisor) handleIdentityMe(w http.ResponseWriter, r *http.Request) {
	session, user, _, ok := s.identityRequest(r)
	if !ok {
		writeIdentityError(w, errIdentityNoSession)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, identityMeResponse{User: user, CSRFToken: session.CSRF})
}

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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request identityPasswordRequest
	if err := json.UnmarshalRead(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var request identityCreateUserRequest
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var request identityResetPasswordRequest
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := s.identity.setPassword(id, request.Password); err != nil {
			writeIdentityError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		user, ok := s.identity.user(id)
		if !ok {
			writeIdentityError(w, errIdentityUserMissing)
			return
		}
		writeJSON(w, http.StatusOK, user)
	case http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var request identityUpdateUserRequest
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
	case http.MethodDelete:
		if err := s.identity.deleteUser(id); err != nil {
			writeIdentityError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
