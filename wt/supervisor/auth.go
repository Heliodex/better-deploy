// Scoped operator and deploy tokens for the admin API.

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	roleAdmin  = "admin"
	roleDeploy = "deploy"
	roleRead   = "read"

	authFileVersion  = 1
	maxAuthTokenName = 64
	maxAuthTokens    = 256
	minTokenExpiry   = time.Minute
	maxTokenExpiry   = 10 * 365 * 24 * time.Hour
)

var errAuthTokenNotFound = errors.New("token not found")

// authIdentity is the caller a request authenticated as. A stored token narrows the apps and actions it may touch.
type authIdentity struct {
	Name string
	Role string
	Apps []string
}

func (i authIdentity) isAdmin() bool {
	return i.Role == roleAdmin
}

func (i authIdentity) canAccessApp(name string) bool {
	if i.isAdmin() {
		return true
	}

	for _, app := range i.Apps {
		if app == "*" || app == name {
			return true
		}
	}

	return false
}

// canPerformAppAction maps the app routes to the read, deploy and admin roles. Deploy tokens cannot change config, secrets, backups, databases or delete an app, and a coordinated rollback with a backup is admin-only because it replaces data.
func (i authIdentity) canPerformAppAction(r *http.Request, action string) bool {
	if i.isAdmin() {
		return true
	}

	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if read {
		return !sensitiveAppRead(action)
	}

	if i.Role != roleDeploy || r.Method != http.MethodPost {
		return false
	}

	switch {
	case action == "", action == "deployments", strings.HasPrefix(action, "deployments/"), strings.HasPrefix(action, "rollback/"):
		if r.URL.Query().Get("backup") != "" {
			return false
		}

		return true
	case action == "restart", action == "stop", action == "start", action == "maintenance":
		return true
	case action == "jobs", strings.HasPrefix(action, "jobs/"), action == "validate":
		return true
	default:
		return false
	}
}

// sensitiveAppRead keeps database contents, backups and secrets to admins even for a read token.
func sensitiveAppRead(action string) bool {
	return action == "secrets" || strings.HasPrefix(action, "secrets/") ||
		action == "database" || strings.HasPrefix(action, "database/") ||
		action == "backups" || strings.HasPrefix(action, "backups/")
}

// authToken is the stored form of a scoped token. Only the SHA-256 of the bearer value is kept.
type authToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"sha256"`
	Role      string    `json:"role"`
	Apps      []string  `json:"apps,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type authTokenFile struct {
	Version int         `json:"version"`
	Tokens  []authToken `json:"tokens"`
}

// authTokenInfo is the public view of a token; the bearer value and its hash never appear.
type authTokenInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Apps      []string   `json:"apps"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

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

// authStore persists hashed scoped tokens under the state directory.
type authStore struct {
	path string

	mu     sync.RWMutex
	tokens []authToken
}

func newAuthStore(path string) (*authStore, error) {
	if path == "" {
		return nil, errors.New("auth file path is empty")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth dir: %w", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth dir: %w", err)
	}

	store := &authStore{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("auth file: %w", err)
	}

	var file authTokenFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("auth file: %w", err)
	}

	if file.Version != authFileVersion {
		return nil, fmt.Errorf("auth file: unsupported version %d", file.Version)
	}

	if len(file.Tokens) > maxAuthTokens {
		return nil, fmt.Errorf("auth file: more than %d tokens", maxAuthTokens)
	}

	for _, token := range file.Tokens {
		if err := validateStoredToken(token); err != nil {
			return nil, fmt.Errorf("auth file: %w", err)
		}
	}

	store.tokens = file.Tokens

	return store, nil
}

// validateStoredToken rejects an auth file entry with a missing field, bad hash or invalid role or apps.
func validateStoredToken(token authToken) error {
	if token.ID == "" || token.Name == "" {
		return errors.New("token has an empty id or name")
	}

	if err := validateRole(token.Role); err != nil {
		return err
	}

	if len(token.Hash) != sha256.Size*2 {
		return fmt.Errorf("token %q has an invalid hash", token.ID)
	}

	if _, err := hex.DecodeString(token.Hash); err != nil {
		return fmt.Errorf("token %q has an invalid hash", token.ID)
	}

	if _, err := normaliseTokenApps(token.Role, token.Apps); err != nil {
		return fmt.Errorf("token %q: %w", token.ID, err)
	}

	return nil
}

// validateRole accepts only the admin, deploy and read roles.
func validateRole(role string) error {
	switch role {
	case roleAdmin, roleDeploy, roleRead:
		return nil
	default:
		return fmt.Errorf("role must be %s, %s or %s", roleAdmin, roleDeploy, roleRead)
	}
}

// normaliseTokenApps validates an app allowlist. Admin tokens imply every app. Other roles need an explicit list or "*".
func normaliseTokenApps(role string, apps []string) ([]string, error) {
	if role == roleAdmin {
		return []string{"*"}, nil
	}

	if len(apps) == 0 {
		return nil, fmt.Errorf("apps is required for a %s token", role)
	}

	out := make([]string, 0, len(apps))
	seen := map[string]struct{}{}

	for _, app := range apps {
		app = strings.ToLower(strings.TrimSpace(app))
		if app == "" {
			continue
		}

		if app != "*" && !validName(app) {
			return nil, fmt.Errorf("%q is not a valid app name", app)
		}

		if _, ok := seen[app]; ok {
			continue
		}

		seen[app] = struct{}{}
		out = append(out, app)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("apps is required for a %s token", role)
	}

	slices.Sort(out)

	return out, nil
}

// tokenHash is the SHA-256 hex digest stored in the auth file.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// secureEqual compares two strings in constant time.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// identity resolves a bearer value to a stored token, rejecting expired entries.
func (s *authStore) identity(token string) (authIdentity, bool) {
	if s == nil || token == "" {
		return authIdentity{}, false
	}

	hash := tokenHash(token)

	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()

	for _, stored := range s.tokens {
		if !secureEqual(hash, stored.Hash) {
			continue
		}

		if !stored.ExpiresAt.IsZero() && !now.Before(stored.ExpiresAt) {
			return authIdentity{}, false
		}

		return authIdentity{Name: stored.Name, Role: stored.Role, Apps: append([]string(nil), stored.Apps...)}, true
	}

	return authIdentity{}, false
}

func (s *authStore) count() int {
	if s == nil {
		return 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.tokens)
}

// list returns token metadata without hashes, sorted by creation time.
func (s *authStore) list() []authTokenInfo {
	if s == nil {
		return []authTokenInfo{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]authTokenInfo, 0, len(s.tokens))
	for _, token := range s.tokens {
		var expiresAt *time.Time

		if !token.ExpiresAt.IsZero() {
			value := token.ExpiresAt
			expiresAt = &value
		}

		out = append(out, authTokenInfo{
			ID:        token.ID,
			Name:      token.Name,
			Role:      token.Role,
			Apps:      append([]string(nil), token.Apps...),
			CreatedAt: token.CreatedAt,
			ExpiresAt: expiresAt,
		})
	}

	slices.SortFunc(out, func(a, b authTokenInfo) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return strings.Compare(a.ID, b.ID)
		}

		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}

		return 1
	})

	return out
}

// create stores a new token and returns its bearer value once.
func (s *authStore) create(name, role string, apps []string, expiresAt time.Time) (string, authTokenInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxAuthTokenName {
		return "", authTokenInfo{}, fmt.Errorf("name must be between 1 and %d bytes", maxAuthTokenName)
	}

	if strings.ContainsAny(name, "\x00\r\n") {
		return "", authTokenInfo{}, errors.New("name contains a control character")
	}

	if err := validateRole(role); err != nil {
		return "", authTokenInfo{}, err
	}

	apps, err := normaliseTokenApps(role, apps)
	if err != nil {
		return "", authTokenInfo{}, err
	}

	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", authTokenInfo{}, fmt.Errorf("generate token: %w", err)
	}

	var idBytes [12]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", authTokenInfo{}, fmt.Errorf("generate token id: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(secret[:])
	now := time.Now().UTC()
	stored := authToken{
		ID:        hex.EncodeToString(idBytes[:]),
		Name:      name,
		Hash:      tokenHash(token),
		Role:      role,
		Apps:      apps,
		CreatedAt: now,
		ExpiresAt: expiresAt.UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tokens) >= maxAuthTokens {
		return "", authTokenInfo{}, fmt.Errorf("at most %d tokens are allowed", maxAuthTokens)
	}

	s.tokens = append(s.tokens, stored)
	if err := s.saveLocked(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		return "", authTokenInfo{}, err
	}

	var expiresInfo *time.Time

	if !stored.ExpiresAt.IsZero() {
		value := stored.ExpiresAt
		expiresInfo = &value
	}

	return token, authTokenInfo{ID: stored.ID, Name: stored.Name, Role: stored.Role, Apps: append([]string(nil), stored.Apps...), CreatedAt: stored.CreatedAt, ExpiresAt: expiresInfo}, nil
}

// remove deletes a token by id and rolls the in-memory list back when saving fails.
func (s *authStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := -1

	for i, token := range s.tokens {
		if token.ID == id {
			index = i
			break
		}
	}

	if index < 0 {
		return errAuthTokenNotFound
	}

	previous := s.tokens
	next := make([]authToken, 0, len(previous)-1)
	next = append(next, previous[:index]...)
	next = append(next, previous[index+1:]...)

	s.tokens = next
	if err := s.saveLocked(); err != nil {
		s.tokens = previous
		return err
	}

	return nil
}

// saveLocked writes the auth file atomically; the caller holds the write lock.
func (s *authStore) saveLocked() error {
	data, err := json.Marshal(authTokenFile{Version: authFileVersion, Tokens: s.tokens})
	if err != nil {
		return err
	}

	if err := writeFileAtomicMode(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write auth file: %w", err)
	}

	return nil
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

func (s *supervisor) authorized(r *http.Request) bool {
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

// tokenFilePath returns the configured auth file or the default under the state directory.
func tokenFilePath(cfg config) string {
	if cfg.authFile != "" {
		return cfg.authFile
	}

	return filepath.Join(cfg.stateDir, ".auth", "tokens.json")
}
