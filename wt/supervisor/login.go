package main

import (
	"crypto/subtle"
	"encoding/json/v2"
	"net/http"
	"strings"
)

const authCookieMaxAge = 365 * 24 * 60 * 60

type loginRequest struct {
	Token string `json:"token"`
}

// setAuthCookie writes the token as an HttpOnly cookie so event streams authenticate without JavaScript access.
func (s *supervisor) setAuthCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     "wt_token",
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// handleLogin checks the deploy token and issues the admin cookie. The panel still sends a bearer token for its own requests, but EventSource can only use the cookie.
func (s *supervisor) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigRequestBytes)
	var req loginRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if s.cfg.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if s.cfg.token == "" {
		token = ""
	}
	w.Header().Set("Cache-Control", "no-store")
	s.setAuthCookie(w, r, token, authCookieMaxAge)
	w.WriteHeader(http.StatusNoContent)
}

func (s *supervisor) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.setAuthCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
