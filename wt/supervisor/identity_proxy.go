// Identity headers and cookies injected when proxying to an app.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// identityUserSignature signs the user values so an app can verify that the supervisor, not the client, supplied them.
func identityUserSignature(secret, id, email, name, groups string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s", id, email, name, groups)

	return hex.EncodeToString(mac.Sum(nil))
}

// identitySecretFor derives an app's identity key from the supervisor's blob secret so it is not shared between apps.
func (s *supervisor) identitySecretFor(app string) string {
	if s.blobs == nil {
		return ""
	}

	mac := hmac.New(sha256.New, []byte(s.blobs.secretFor(app)))
	_, _ = mac.Write([]byte("identity"))

	return hex.EncodeToString(mac.Sum(nil))
}

// identityRequest resolves the session cookie and returns the user and raw token.
func (s *supervisor) identityRequest(r *http.Request) (identitySession, identityUserInfo, string, bool) {
	if s.identity == nil {
		return identitySession{}, identityUserInfo{}, "", false
	}

	cookie, err := r.Cookie(identityCookieName)
	if err != nil || cookie.Value == "" {
		return identitySession{}, identityUserInfo{}, "", false
	}

	session, user, err := s.identity.session(cookie.Value)
	if err != nil {
		return identitySession{}, identityUserInfo{}, "", false
	}

	return session, user, cookie.Value, true
}

// identityProxyHeaders removes any client-supplied identity values and injects the signed platform user for an opted-in app.
func (s *supervisor) identityProxyHeaders(r *http.Request, appName string, enabled bool) {
	for key := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), http.CanonicalHeaderKey(identityHeaderPrefix)) {
			r.Header.Del(key)
		}
	}

	var (
		user identityUserInfo
		ok   bool
	)
	if enabled {
		_, user, _, ok = s.identityRequest(r)
	}
	// The app never receives the supervisor session cookie; it gets signed headers instead.
	stripIdentityCookie(r)

	if !ok {
		return
	}

	groups := strings.Join(user.Groups, ",")

	secret := s.identitySecretFor(appName)
	if secret == "" {
		return
	}

	r.Header.Set(identityUserIDHeader, user.ID)
	r.Header.Set(identityUserEmail, user.Email)
	r.Header.Set(identityUserName, user.Name)
	r.Header.Set(identityUserGroups, groups)
	r.Header.Set(identityUserSignatureHeader, identityUserSignature(secret, user.ID, user.Email, user.Name, groups))
}

// stripIdentityCookie removes only the supervisor session cookie before proxying.
func stripIdentityCookie(r *http.Request) {
	raw := r.Header.Get("Cookie")
	if raw == "" {
		return
	}

	parts := strings.Split(raw, ";")

	kept := parts[:0]
	for _, part := range parts {
		name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		if name == identityCookieName {
			continue
		}

		kept = append(kept, part)
	}

	if len(kept) == 0 {
		r.Header.Del("Cookie")
		return
	}

	r.Header.Set("Cookie", strings.Join(kept, "; "))
}

func (s *supervisor) setIdentityCookie(w http.ResponseWriter, r *http.Request, token string) {
	s.setSessionCookie(w, r, identityCookieName, token, int(identitySessionTTL/time.Second))
}

func (s *supervisor) clearIdentityCookie(w http.ResponseWriter, r *http.Request) {
	s.setSessionCookie(w, r, identityCookieName, "", -1)
}
