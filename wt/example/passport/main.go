package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"wt.example/passport/contract"
	"wt.packages/app"
	"wt.packages/storage"
)

// errNoSession distinguishes a missing cookie from a storage failure when loading a session.
var errNoSession = errors.New("no session")

const (
	maxMessage    = 280
	maxBody       = 4 << 10
	defaultLimit  = 50
	maxLimit      = 100
	sessionTTL    = 8 * time.Hour
	flowTTL       = 10 * time.Minute
	sessionCookie = "passport_session"
	stateCookie   = "passport_oauth_state"
)

// passportApp owns the store, OAuth client and routes so handler tests can build one without wasmtime.
type passportApp struct {
	store *storage.Client
	mux   *http.ServeMux
	oauth *oauthClient
}

// profile is the identity a successful OAuth login leaves in a session.
type profile struct {
	Sub, Name, Email, Username, Picture, Zoneinfo string
}

// session is one signed-in browser, loaded from the hashed cookie token on each request.
type session struct {
	profile   profile
	expiresAt time.Time
	idToken   string
}

// sessionResponseEnv builds the public session response, with empty user fields when signed out.
func (a *passportApp) sessionResponse(sess *session) contract.SessionResponse {
	response := contract.SessionResponse{
		Authenticated:  sess != nil,
		ClientID:       a.oauth.cfg.clientID,
		ProviderIssuer: a.oauth.cfg.issuer,
		ProviderName:   a.oauth.cfg.providerName,
		Scopes:         a.oauth.cfg.scopes,
	}

	if sess == nil {
		return response
	}

	response.ExpiresAt = sess.expiresAt.UTC().Format(time.RFC3339)
	response.UserEmail = sess.profile.Email
	response.UserName = sess.profile.Name
	response.UserPicture = sess.profile.Picture
	response.UserSub = sess.profile.Sub
	response.UserUsername = sess.profile.Username
	response.UserZoneinfo = sess.profile.Zoneinfo

	return response
}

// hashToken keeps only a digest of a bearer token in the database.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// newToken returns a URL-safe random token with n bytes of entropy.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// secureCookie reports whether the browser reached the app over HTTPS.
func secureCookie(r *http.Request) bool {
	if r.TLS != nil || r.URL.Scheme == "https" {
		return true
	}

	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setCookie writes one HttpOnly cookie scoped to the whole app.
func setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	//nolint:gosec // Secure follows the request scheme; HttpOnly and SameSite are set.
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie expires a cookie the login flow set.
func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	//nolint:gosec // Secure follows the request scheme; HttpOnly and SameSite are set.
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// noStore keeps tokens and session state out of browser and intermediary caches.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// sameOrigin rejects cross-site state-changing requests even when SameSite is relaxed by an old browser.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Host, r.Host)
}

// redirect sends the browser to a path on this app.
func redirect(w http.ResponseWriter, r *http.Request, location string) {
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// authFailed sends the browser back to the frontend with a short reason the page can explain.
func authFailed(w http.ResponseWriter, r *http.Request, reason string) {
	redirect(w, r, "/?auth="+url.QueryEscape(reason))
}

// loadSession looks up the session cookie, returning nil for no session or an expired one.
func (a *passportApp) loadSession(ctx context.Context, r *http.Request) (*session, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, errNoSession
	}

	var (
		sess      session
		expiresAt string
	)

	err = a.store.QueryRow(ctx, `select user_sub, user_name, user_email, user_username, user_picture, user_zoneinfo, id_token, expires_at
		from sessions where id = ? and expires_at > ?`, hashToken(cookie.Value), time.Now().UTC().Format(app.TimestampFormat)).
		Scan(&sess.profile.Sub, &sess.profile.Name, &sess.profile.Email, &sess.profile.Username,
			&sess.profile.Picture, &sess.profile.Zoneinfo, &sess.idToken, &expiresAt)
	if errors.Is(err, storage.ErrNoRows) {
		return nil, errNoSession
	}

	if err != nil {
		return nil, err
	}

	parsed, err := time.Parse(app.TimestampFormat, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse session expiry: %w", err)
	}

	sess.expiresAt = parsed

	return &sess, nil
}

// requireSession loads the session or writes a 401 and returns nil.
func (a *passportApp) requireSession(w http.ResponseWriter, r *http.Request) *session {
	sess, err := a.loadSession(r.Context(), r)
	if errors.Is(err, errNoSession) {
		app.WriteErrorCode(w, http.StatusUnauthorized, "unauthorized", "sign in first")
		return nil
	}

	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, "could not load the session")
		return nil
	}

	return sess
}

func (a *passportApp) handleSession(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	sess, err := a.loadSession(r.Context(), r)
	if err != nil && !errors.Is(err, errNoSession) {
		app.WriteError(w, http.StatusInternalServerError, "could not load the session")
		return
	}

	if errors.Is(err, errNoSession) {
		sess = nil
	}

	app.WriteJSON(w, http.StatusOK, a.sessionResponse(sess))
}

// listCheckins returns the newest entries and marks the ones the current user wrote.
func (a *passportApp) listCheckins(ctx context.Context, sess *session, limit int) ([]contract.Checkin, error) {
	rows, err := a.store.Query(ctx, `select id, message, author_sub, author_name, author_picture, created_at
		from checkins order by created_at desc, id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	checkins := make([]contract.Checkin, 0, limit)

	for rows.Next() {
		var entry contract.Checkin
		if err := rows.Scan(&entry.ID, &entry.Message, &entry.AuthorSub, &entry.AuthorName, &entry.AuthorPicture, &entry.CreatedAt); err != nil {
			return nil, err
		}

		entry.Mine = sess != nil && entry.AuthorSub == sess.profile.Sub
		checkins = append(checkins, entry)
	}

	return checkins, rows.Err()
}

func (a *passportApp) handleListCheckins(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit

	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			app.WriteErrorField(w, http.StatusBadRequest, "invalid", "limit", "limit must be a positive integer")
			return
		}

		limit = min(parsed, maxLimit)
	}

	sess, err := a.loadSession(r.Context(), r)
	if err != nil && !errors.Is(err, errNoSession) {
		app.WriteError(w, http.StatusInternalServerError, "could not load the session")
		return
	}

	if errors.Is(err, errNoSession) {
		sess = nil
	}

	checkins, err := a.listCheckins(r.Context(), sess, limit)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, "could not load check-ins")
		return
	}

	app.WriteJSON(w, http.StatusOK, contract.CheckinsResponse{Checkins: checkins})
}

// validMessage reports whether a check-in is non-blank and within the character limit.
func validMessage(message string) string {
	if message == "" {
		return "Write a few words."
	}

	if !utf8.ValidString(message) {
		return "The message is not valid UTF-8."
	}

	if utf8.RuneCountInString(message) > maxMessage {
		return fmt.Sprintf("Check-ins are limited to %d characters.", maxMessage)
	}

	return ""
}

func (a *passportApp) handleCreateCheckin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		app.WriteErrorCode(w, http.StatusForbidden, "forbidden", "cross-site request rejected")
		return
	}

	sess := a.requireSession(w, r)
	if sess == nil {
		return
	}

	var request contract.CreateCheckinRequest
	if !app.ReadJSON(w, r, maxBody, "message", &request) {
		return
	}

	message := strings.TrimSpace(request.Message)
	if problem := validMessage(message); problem != "" {
		app.WriteErrorField(w, http.StatusBadRequest, "invalid", "message", problem)
		return
	}

	entry := contract.Checkin{
		AuthorName:    displayName(sess.profile),
		AuthorPicture: sess.profile.Picture,
		AuthorSub:     sess.profile.Sub,
		CreatedAt:     time.Now().UTC().Format(app.TimestampFormat),
		ID:            app.NewID(),
		Message:       message,
		Mine:          true,
	}

	if _, err := a.store.Exec(r.Context(), `insert into checkins (id, message, author_sub, author_name, author_picture, created_at)
		values (?, ?, ?, ?, ?, ?)`, entry.ID, entry.Message, entry.AuthorSub, entry.AuthorName, entry.AuthorPicture, entry.CreatedAt); err != nil {
		app.WriteError(w, http.StatusInternalServerError, "could not save the check-in")
		return
	}

	app.WriteJSON(w, http.StatusCreated, entry)
}

func (a *passportApp) handleDeleteCheckin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		app.WriteErrorCode(w, http.StatusForbidden, "forbidden", "cross-site request rejected")
		return
	}

	sess := a.requireSession(w, r)
	if sess == nil {
		return
	}

	result, err := a.store.Exec(r.Context(), `delete from checkins where id = ? and author_sub = ?`, r.PathValue("id"), sess.profile.Sub)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, "could not delete the check-in")
		return
	}

	if result.RowsAffected == 0 {
		app.WriteError(w, http.StatusNotFound, "check-in not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *passportApp) handleLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	if !sameOrigin(r) {
		app.WriteErrorCode(w, http.StatusForbidden, "forbidden", "cross-site request rejected")
		return
	}

	response := contract.LogoutResponse{}

	cookie, err := r.Cookie(sessionCookie)
	if err == nil && cookie.Value != "" {
		sess, loadErr := a.loadSession(r.Context(), r)
		if loadErr != nil && !errors.Is(loadErr, errNoSession) {
			app.WriteError(w, http.StatusInternalServerError, "could not load the session")
			return
		}

		if _, err := a.store.Exec(r.Context(), `delete from sessions where id = ?`, hashToken(cookie.Value)); err != nil {
			app.WriteError(w, http.StatusInternalServerError, "could not sign out")
			return
		}

		if !errors.Is(loadErr, errNoSession) {
			response.EndSessionURL = a.endSessionURL(r, sess)
		}
	}

	clearCookie(w, r, sessionCookie)
	app.WriteJSON(w, http.StatusOK, response)
}

// endSessionURL builds the RP-initiated logout redirect from the discovery document already cached by login, or an empty string when there is none.
func (a *passportApp) endSessionURL(r *http.Request, sess *session) string {
	metadata, ok := a.oauth.cachedMetadata()
	if !ok || metadata.EndSessionEndpoint == "" {
		return ""
	}

	params := url.Values{"client_id": {a.oauth.cfg.clientID}}
	if sess != nil && sess.idToken != "" {
		params.Set("id_token_hint", sess.idToken)
	}

	if base := a.oauth.baseURL(r); base != "" {
		params.Set("post_logout_redirect_uri", base+"/")
	}

	return metadata.EndSessionEndpoint + "?" + params.Encode()
}

// handleLogin starts the authorization code flow with PKCE and a matching state cookie.
func (a *passportApp) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		app.WriteErrorCode(w, http.StatusForbidden, "forbidden", "cross-site request rejected")
		return
	}

	metadata, err := a.oauth.discover(r.Context())
	if err != nil {
		log.Printf("oauth discovery: %v", err)
		authFailed(w, r, "provider")

		return
	}

	state, err := newToken()
	if err != nil {
		authFailed(w, r, "internal")
		return
	}

	verifier, challenge, err := newPKCE()
	if err != nil {
		authFailed(w, r, "internal")
		return
	}

	nonce, err := newToken()
	if err != nil {
		authFailed(w, r, "internal")
		return
	}

	redirectURI := a.oauth.baseURL(r) + "/auth/callback"

	_, err = a.store.Batch(r.Context(),
		storage.Statement{
			SQL:  `delete from login_flows where created_at < ?`,
			Args: []any{time.Now().UTC().Add(-flowTTL).Format(app.TimestampFormat)},
		},
		storage.Statement{
			SQL:  `insert into login_flows (state, verifier, nonce, redirect_uri, created_at) values (?, ?, ?, ?, ?)`,
			Args: []any{state, verifier, nonce, redirectURI, time.Now().UTC().Format(app.TimestampFormat)},
		},
	)
	if err != nil {
		log.Printf("store login flow: %v", err)
		authFailed(w, r, "internal")

		return
	}

	setCookie(w, r, stateCookie, state, flowTTL)

	params := url.Values{
		"client_id":             {a.oauth.cfg.clientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"nonce":                 {nonce},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(a.oauth.cfg.scopes, " ")},
		"state":                 {state},
	}
	http.Redirect(w, r, metadata.AuthorizationEndpoint+"?"+params.Encode(), http.StatusFound)
}

// callbackFlow is the stored half of one login attempt.
type callbackFlow struct {
	verifier, nonce, redirectURI string
}

// takeFlow consumes a login flow so a callback code cannot be replayed.
func (a *passportApp) takeFlow(ctx context.Context, state string) (callbackFlow, error) {
	var (
		flow      callbackFlow
		createdAt string
	)

	err := a.store.QueryRow(ctx, `select verifier, nonce, redirect_uri, created_at from login_flows where state = ?`, state).
		Scan(&flow.verifier, &flow.nonce, &flow.redirectURI, &createdAt)
	if err != nil {
		return flow, err
	}

	if _, err := a.store.Exec(ctx, `delete from login_flows where state = ?`, state); err != nil {
		return flow, err
	}

	if created, err := time.Parse(app.TimestampFormat, createdAt); err != nil || time.Since(created) > flowTTL {
		return flow, errors.New("login flow expired")
	}

	return flow, nil
}

// handleCallback validates state, exchanges the code, verifies the ID token and opens a session. The static /auth/callback page calls it, because the supervisor serves HTML navigations from static files before the component sees them.
func (a *passportApp) handleCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	if !sameOrigin(r) {
		app.WriteErrorCode(w, http.StatusForbidden, "forbidden", "cross-site request rejected")
		return
	}

	var request contract.CallbackRequest
	if !app.ReadJSON(w, r, maxBody, "state", &request) {
		return
	}

	cookie, err := r.Cookie(stateCookie)
	if err != nil || cookie.Value == "" || request.State == "" || cookie.Value != request.State {
		app.WriteErrorCode(w, http.StatusBadRequest, "state", "the login state did not match this browser")
		return
	}

	flow, err := a.takeFlow(r.Context(), request.State)
	if errors.Is(err, storage.ErrNoRows) {
		app.WriteErrorCode(w, http.StatusBadRequest, "expired", "the login flow expired or was already used")
		return
	}

	if err != nil {
		log.Printf("take login flow: %v", err)
		app.WriteErrorCode(w, http.StatusInternalServerError, "internal", "could not complete the login")

		return
	}

	metadata, err := a.oauth.discover(r.Context())
	if err != nil {
		log.Printf("oauth discovery: %v", err)
		app.WriteErrorCode(w, http.StatusBadGateway, "provider", "the identity provider could not be reached")

		return
	}

	tokens, err := a.oauth.exchange(r.Context(), metadata, request.Code, flow)
	if err != nil {
		log.Printf("oauth token exchange: %v", err)
		app.WriteErrorCode(w, http.StatusBadGateway, "exchange", "the identity provider rejected the code exchange")

		return
	}

	claims, err := a.oauth.verifyIDToken(r.Context(), metadata, tokens.IDToken, flow.nonce)
	if err != nil {
		log.Printf("verify id token: %v", err)
		app.WriteErrorCode(w, http.StatusBadGateway, "token", "the identity token could not be verified")

		return
	}

	if userinfo, err := a.oauth.userinfo(r.Context(), metadata, tokens.AccessToken); err == nil {
		claims.merge(userinfo)
	} else {
		log.Printf("oauth userinfo: %v", err)
	}

	user := claims.profile()
	if user.Sub == "" {
		app.WriteErrorCode(w, http.StatusBadGateway, "token", "the identity token has no subject")
		return
	}

	token, err := newToken()
	if err != nil {
		app.WriteErrorCode(w, http.StatusInternalServerError, "internal", "could not create a session")
		return
	}

	now := time.Now().UTC()
	expiresAt := now.Add(sessionTTL)

	_, err = a.store.Batch(r.Context(),
		storage.Statement{
			SQL:  `delete from sessions where expires_at <= ?`,
			Args: []any{now.Format(app.TimestampFormat)},
		},
		storage.Statement{
			SQL: `insert into sessions (id, user_sub, user_name, user_email, user_username, user_picture, user_zoneinfo,
				access_token, refresh_token, id_token, created_at, expires_at)
				values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			Args: []any{hashToken(token), user.Sub, user.Name, user.Email, user.Username, user.Picture, user.Zoneinfo,
				tokens.AccessToken, tokens.RefreshToken, tokens.IDToken,
				now.Format(app.TimestampFormat), expiresAt.Format(app.TimestampFormat)},
		},
	)
	if err != nil {
		log.Printf("store session: %v", err)
		app.WriteErrorCode(w, http.StatusInternalServerError, "internal", "could not store the session")

		return
	}

	setCookie(w, r, sessionCookie, token, sessionTTL)
	clearCookie(w, r, stateCookie)

	w.WriteHeader(http.StatusNoContent)
}

// displayName falls back through the claims the provider may have sent.
func displayName(user profile) string {
	for _, value := range []string{user.Name, user.Username, user.Email, user.Sub} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return "Guest"
}

// envDefault returns a trimmed environment value or a fallback.
func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	return fallback
}

// migrate creates the login-flow, session and check-in tables.
func (a *passportApp) migrate(ctx context.Context) error {
	return a.store.Migrate(ctx, 1,
		storage.Statement{SQL: `create table if not exists login_flows (
			state text primary key,
			verifier text not null,
			nonce text not null,
			redirect_uri text not null,
			created_at text not null
		)`},
		storage.Statement{SQL: `create table if not exists sessions (
			id text primary key,
			user_sub text not null,
			user_name text not null,
			user_email text not null,
			user_username text not null,
			user_picture text not null,
			user_zoneinfo text not null,
			access_token text not null,
			refresh_token text not null,
			id_token text not null,
			created_at text not null,
			expires_at text not null
		)`},
		storage.Statement{SQL: `create table if not exists checkins (
			id text primary key,
			message text not null,
			author_sub text not null,
			author_name text not null,
			author_picture text not null,
			created_at text not null
		)`},
		storage.Statement{SQL: `create index if not exists checkins_created_idx on checkins(created_at)`},
		storage.Statement{SQL: `create index if not exists sessions_expires_idx on sessions(expires_at)`},
	)
}

// handler exposes the app's routes to the wasm wiring or a test.
func (a *passportApp) handler() http.Handler {
	return a.mux
}

// newPassportApp builds the app around one store and one HTTP client.
func newPassportApp(store *storage.Client, client *http.Client) *passportApp {
	a := &passportApp{store: store, mux: http.NewServeMux()}
	a.oauth = newOAuthClient(client)

	a.mux.HandleFunc("GET /api/session", a.handleSession)
	a.mux.HandleFunc("GET /api/checkins", a.handleListCheckins)
	a.mux.HandleFunc("POST /api/checkins", a.handleCreateCheckin)
	a.mux.HandleFunc("DELETE /api/checkins/{id}", a.handleDeleteCheckin)
	a.mux.HandleFunc("POST /api/logout", a.handleLogout)
	a.mux.HandleFunc("POST /auth/login", a.handleLogin)
	a.mux.HandleFunc("POST /api/auth/callback", a.handleCallback)

	return a
}

// scopesFromEnv keeps the requested scopes in a stable, deduplicated order.
func scopesFromEnv() []string {
	scopes := strings.Fields(envDefault("OAUTH_SCOPES", "openid profile email"))
	slices.Sort(scopes)

	return slices.Compact(scopes)
}

func main() {}
