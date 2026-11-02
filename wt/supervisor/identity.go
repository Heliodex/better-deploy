// Platform users, password hashing and persisted sessions.
// Apps opt in with app.json identity: "platform"; the supervisor then injects signed user headers on proxied requests.

package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
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
	identityDirName             = ".identity"
	identityUsersName           = "users.json"
	identitySessionsName        = "sessions.json"
	identityModePlatform        = "platform"
	identityCookieName          = "wt_user"
	identityCSRFHeader          = "X-CSRF-Token"
	identitySessionTTL          = 30 * 24 * time.Hour
	identityPasswordIter        = 600_000
	identityPasswordMin         = 10
	identityPasswordMax         = 1024
	identityEmailMax            = 254
	identityNameMax             = 100
	identityGroupMax            = 32
	identityGroupBytes          = 64
	identityMaxUsers            = 10_000
	identityMaxSessions         = 100_000
	identityVersion             = 1
	identitySecretEnv           = "WT_IDENTITY_SECRET"
	identityUserIDHeader        = "X-WT-User-Id"
	identityUserEmail           = "X-WT-User-Email"
	identityUserName            = "X-WT-User-Name"
	identityUserGroups          = "X-WT-User-Groups"
	identityUserSignatureHeader = "X-WT-User-Signature"
	identityHeaderPrefix        = "X-WT-User-"
)

var (
	errIdentityUserExists  = errors.New("a user with that email already exists")
	errIdentityNoUser      = errors.New("unknown email or password")
	errIdentityDisabled    = errors.New("this user is disabled")
	errIdentityNoSession   = errors.New("sign in required")
	errIdentityCSRF        = errors.New("invalid CSRF token")
	errIdentityUserMissing = errors.New("user not found")
	errIdentityPassword    = errors.New("password must be between 10 and 1024 characters")
	errIdentityEmail       = errors.New("enter a valid email address")
	errIdentityName        = errors.New("enter a name")
	errIdentityGroups      = errors.New("groups are invalid")
)

// identityUser is the persisted record. Only a PBKDF2 hash is stored.
type identityUser struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Groups    []string  `json:"groups,omitempty"`
	Password  string    `json:"password"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type identitySession struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	TokenHash  string    `json:"tokenHash"`
	CSRF       string    `json:"csrf"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

type identityUserFile struct {
	Version int            `json:"version"`
	Users   []identityUser `json:"users"`
}

type identitySessionFile struct {
	Version  int               `json:"version"`
	Sessions []identitySession `json:"sessions"`
}

// identityUserInfo is the public view; it never includes the password hash.
type identityUserInfo struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Groups    []string  `json:"groups"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (u identityUser) info() identityUserInfo {
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	return identityUserInfo{ID: u.ID, Email: u.Email, Name: u.Name, Groups: groups, Disabled: u.Disabled, CreatedAt: u.CreatedAt}
}

type identityStore struct {
	dir string

	mu       sync.Mutex
	users    map[string]*identityUser
	byEmail  map[string]string
	sessions map[string]*identitySession
}

func newIdentityStore(stateDir string) (*identityStore, error) {
	dir := filepath.Join(stateDir, identityDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identity dir: %w", err)
	}
	store := &identityStore{dir: dir, users: map[string]*identityUser{}, byEmail: map[string]string{}, sessions: map[string]*identitySession{}}
	if err := store.loadUsers(); err != nil {
		return nil, err
	}
	if err := store.loadSessions(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *identityStore) usersPath() string {
	return filepath.Join(s.dir, identityUsersName)
}

func (s *identityStore) sessionsPath() string {
	return filepath.Join(s.dir, identitySessionsName)
}

func (s *identityStore) loadUsers() error {
	data, err := os.ReadFile(s.usersPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read users: %w", err)
	}
	if err := os.Chmod(s.usersPath(), 0o600); err != nil {
		return fmt.Errorf("users permissions: %w", err)
	}
	var file identityUserFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("users file: %w", err)
	}
	if file.Version != identityVersion {
		return fmt.Errorf("users file: unsupported version %d", file.Version)
	}
	if len(file.Users) > identityMaxUsers {
		return fmt.Errorf("users file: more than %d users", identityMaxUsers)
	}
	for i := range file.Users {
		user := &file.Users[i]
		if user.ID == "" || user.Email == "" || user.Password == "" {
			return fmt.Errorf("users file: user %q is incomplete", user.ID)
		}
		if _, exists := s.byEmail[user.Email]; exists {
			return fmt.Errorf("users file: duplicate email %q", user.Email)
		}
		s.users[user.ID] = user
		s.byEmail[user.Email] = user.ID
	}
	return nil
}

func (s *identityStore) loadSessions() error {
	data, err := os.ReadFile(s.sessionsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}
	if err := os.Chmod(s.sessionsPath(), 0o600); err != nil {
		return fmt.Errorf("sessions permissions: %w", err)
	}
	var file identitySessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("sessions file: %w", err)
	}
	if file.Version != identityVersion {
		return fmt.Errorf("sessions file: unsupported version %d", file.Version)
	}
	if len(file.Sessions) > identityMaxSessions {
		return fmt.Errorf("sessions file: more than %d sessions", identityMaxSessions)
	}
	now := time.Now()
	changed := false
	for i := range file.Sessions {
		session := &file.Sessions[i]
		if session.TokenHash == "" || session.UserID == "" || session.CSRF == "" {
			continue
		}
		if session.ExpiresAt.Before(now) {
			changed = true
			continue
		}
		if _, ok := s.users[session.UserID]; !ok {
			changed = true
			continue
		}
		s.sessions[session.TokenHash] = session
	}
	if changed {
		return s.saveSessionsLocked()
	}
	return nil
}

func (s *identityStore) saveUsersLocked() error {
	ids := make([]string, 0, len(s.users))
	for id := range s.users {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	users := make([]identityUser, 0, len(ids))
	for _, id := range ids {
		users = append(users, *s.users[id])
	}
	data, err := json.Marshal(identityUserFile{Version: identityVersion, Users: users})
	if err != nil {
		return err
	}
	return writeFileAtomicMode(s.usersPath(), append(data, '\n'), 0o600)
}

func (s *identityStore) saveSessionsLocked() error {
	hashes := make([]string, 0, len(s.sessions))
	for hash := range s.sessions {
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes)
	sessions := make([]identitySession, 0, len(hashes))
	for _, hash := range hashes {
		sessions = append(sessions, *s.sessions[hash])
	}
	data, err := json.Marshal(identitySessionFile{Version: identityVersion, Sessions: sessions})
	if err != nil {
		return err
	}
	return writeFileAtomicMode(s.sessionsPath(), append(data, '\n'), 0o600)
}

func normalizeIdentityEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > identityEmailMax || strings.Count(value, "@") != 1 {
		return "", errIdentityEmail
	}
	local, domain, _ := strings.Cut(value, "@")
	if local == "" || domain == "" || strings.ContainsAny(value, " \t\r\n") || !strings.Contains(domain, ".") {
		return "", errIdentityEmail
	}
	return value, nil
}

func normalizeIdentityName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > identityNameMax {
		return "", errIdentityName
	}
	return value, nil
}

func normalizeIdentityGroups(groups []string) ([]string, error) {
	if len(groups) > identityGroupMax {
		return nil, errIdentityGroups
	}
	out := make([]string, 0, len(groups))
	seen := map[string]struct{}{}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if len(group) > identityGroupBytes || strings.ContainsAny(group, ",\t\r\n") {
			return nil, errIdentityGroups
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		out = append(out, group)
	}
	slices.Sort(out)
	return out, nil
}

func validateIdentityPassword(password string) error {
	if len(password) < identityPasswordMin || len(password) > identityPasswordMax {
		return errIdentityPassword
	}
	return nil
}

func hashIdentityPassword(password string) (string, error) {
	if err := validateIdentityPassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, identityPasswordIter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", identityPasswordIter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyIdentityPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func newIdentityToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func identityTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *identityStore) createUser(email, name, password string, groups []string) (identityUserInfo, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return identityUserInfo{}, err
	}
	name, err = normalizeIdentityName(name)
	if err != nil {
		return identityUserInfo{}, err
	}
	groups, err = normalizeIdentityGroups(groups)
	if err != nil {
		return identityUserInfo{}, err
	}
	hash, err := hashIdentityPassword(password)
	if err != nil {
		return identityUserInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.users) >= identityMaxUsers {
		return identityUserInfo{}, fmt.Errorf("at most %d users are allowed", identityMaxUsers)
	}
	if _, ok := s.byEmail[email]; ok {
		return identityUserInfo{}, errIdentityUserExists
	}
	id, err := newIdentityToken(12)
	if err != nil {
		return identityUserInfo{}, err
	}
	now := time.Now().UTC()
	user := &identityUser{ID: "usr_" + id, Email: email, Name: name, Groups: groups, Password: hash, CreatedAt: now, UpdatedAt: now}
	s.users[user.ID] = user
	s.byEmail[email] = user.ID
	if err := s.saveUsersLocked(); err != nil {
		delete(s.users, user.ID)
		delete(s.byEmail, email)
		return identityUserInfo{}, err
	}
	return user.info(), nil
}

func (s *identityStore) listUsers() []identityUserInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.users))
	for id := range s.users {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]identityUserInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.users[id].info())
	}
	return out
}

func (s *identityStore) user(id string) (identityUserInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return identityUserInfo{}, false
	}
	return user.info(), true
}

func (s *identityStore) updateUser(id, name string, groups []string, disabled *bool) (identityUserInfo, error) {
	name, err := normalizeIdentityName(name)
	if err != nil {
		return identityUserInfo{}, err
	}
	groups, err = normalizeIdentityGroups(groups)
	if err != nil {
		return identityUserInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return identityUserInfo{}, errIdentityUserMissing
	}
	user.Name = name
	user.Groups = groups
	user.UpdatedAt = time.Now().UTC()
	if disabled != nil {
		user.Disabled = *disabled
		if *disabled {
			s.deleteUserSessionsLocked(id)
		}
	}
	if err := s.saveUsersLocked(); err != nil {
		return identityUserInfo{}, err
	}
	return user.info(), nil
}

func (s *identityStore) setPassword(id, password string) error {
	hash, err := hashIdentityPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return errIdentityUserMissing
	}
	user.Password = hash
	user.UpdatedAt = time.Now().UTC()
	s.deleteUserSessionsLocked(id)
	if err := s.saveUsersLocked(); err != nil {
		return err
	}
	return s.saveSessionsLocked()
}

func (s *identityStore) deleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return errIdentityUserMissing
	}
	delete(s.users, id)
	delete(s.byEmail, user.Email)
	s.deleteUserSessionsLocked(id)
	if err := s.saveUsersLocked(); err != nil {
		return err
	}
	return s.saveSessionsLocked()
}

func (s *identityStore) deleteUserSessionsLocked(userID string) {
	for hash, session := range s.sessions {
		if session.UserID == userID {
			delete(s.sessions, hash)
		}
	}
}

func (s *identityStore) authenticate(email, password string) (identityUserInfo, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return identityUserInfo{}, errIdentityNoUser
	}
	s.mu.Lock()
	id, ok := s.byEmail[email]
	if !ok {
		s.mu.Unlock()
		return identityUserInfo{}, errIdentityNoUser
	}
	user := s.users[id]
	encoded := user.Password
	disabled := user.Disabled
	info := user.info()
	s.mu.Unlock()
	if disabled {
		return identityUserInfo{}, errIdentityDisabled
	}
	if !verifyIdentityPassword(encoded, password) {
		return identityUserInfo{}, errIdentityNoUser
	}
	return info, nil
}

// newSession returns the raw token and CSRF value; only the token hash is stored.
func (s *identityStore) newSession(userID string) (string, *identitySession, error) {
	token, err := newIdentityToken(32)
	if err != nil {
		return "", nil, err
	}
	csrf, err := newIdentityToken(16)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	session := &identitySession{
		ID:         "ses_" + csrf[:16],
		UserID:     userID,
		TokenHash:  identityTokenHash(token),
		CSRF:       csrf,
		CreatedAt:  now,
		ExpiresAt:  now.Add(identitySessionTTL),
		LastSeenAt: now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the store bounded by dropping expired sessions whenever a new one is created.
	if len(s.sessions) >= identityMaxSessions {
		for hash, existing := range s.sessions {
			if existing.ExpiresAt.Before(now) {
				delete(s.sessions, hash)
			}
		}
	}
	s.sessions[session.TokenHash] = session
	if err := s.saveSessionsLocked(); err != nil {
		delete(s.sessions, session.TokenHash)
		return "", nil, err
	}
	return token, session, nil
}

func (s *identityStore) session(token string) (identitySession, identityUserInfo, error) {
	if token == "" {
		return identitySession{}, identityUserInfo{}, errIdentityNoSession
	}
	hash := identityTokenHash(token)
	now := time.Now().UTC()
	s.mu.Lock()
	session, ok := s.sessions[hash]
	if !ok {
		s.mu.Unlock()
		return identitySession{}, identityUserInfo{}, errIdentityNoSession
	}
	if session.ExpiresAt.Before(now) {
		delete(s.sessions, hash)
		_ = s.saveSessionsLocked()
		s.mu.Unlock()
		return identitySession{}, identityUserInfo{}, errIdentityNoSession
	}
	session.LastSeenAt = now
	user, ok := s.users[session.UserID]
	if !ok || user.Disabled {
		delete(s.sessions, hash)
		_ = s.saveSessionsLocked()
		s.mu.Unlock()
		return identitySession{}, identityUserInfo{}, errIdentityNoSession
	}
	copy := *session
	info := user.info()
	s.mu.Unlock()
	return copy, info, nil
}

func (s *identityStore) deleteSession(token string) error {
	if token == "" {
		return nil
	}
	hash := identityTokenHash(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[hash]; !ok {
		return nil
	}
	delete(s.sessions, hash)
	return s.saveSessionsLocked()
}

// identityHeaders signs the user values so an app can verify that the supervisor, not the client, supplied them.
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
	var user identityUserInfo
	var ok bool
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

func (s *supervisor) identityCookieSecure(r *http.Request) bool {
	return s.forwardedInfo(r).Proto == "https"
}

func (s *supervisor) setIdentityCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     identityCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(identitySessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.identityCookieSecure(r),
	})
}

func (s *supervisor) clearIdentityCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     identityCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.identityCookieSecure(r),
	})
}
