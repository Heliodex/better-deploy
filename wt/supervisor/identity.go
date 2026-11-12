// Platform users, password hashing and persisted sessions.
// Apps opt in with app.json identity: "platform"; the supervisor then injects signed user headers on proxied requests.

package main

import (
	"errors"
	"fmt"
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
	identityCSRFHeader          = "X-Csrf-Token"
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
	identitySecretEnv           = "WT_IDENTITY_SECRET" //nolint:gosec // Environment variable name, not a credential.
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

// normaliseIdentityEmail lowercases and validates a platform user email.
func normaliseIdentityEmail(value string) (string, error) {
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

// normaliseIdentityName trims and bounds a display name.
func normaliseIdentityName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > identityNameMax {
		return "", errIdentityName
	}

	return value, nil
}

// normaliseIdentityGroups trims, deduplicates and sorts group names.
func normaliseIdentityGroups(groups []string) ([]string, error) {
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

func (s *identityStore) createUser(email, name, password string, groups []string) (identityUserInfo, error) {
	email, err := normaliseIdentityEmail(email)
	if err != nil {
		return identityUserInfo{}, err
	}

	name, err = normaliseIdentityName(name)
	if err != nil {
		return identityUserInfo{}, err
	}

	groups, err = normaliseIdentityGroups(groups)
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
	name, err := normaliseIdentityName(name)
	if err != nil {
		return identityUserInfo{}, err
	}

	groups, err = normaliseIdentityGroups(groups)
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

// deleteUserSessionsLocked removes every session for one user; the caller holds the lock.
func (s *identityStore) deleteUserSessionsLocked(userID string) {
	for hash, session := range s.sessions {
		if session.UserID == userID {
			delete(s.sessions, hash)
		}
	}
}

func (s *identityStore) authenticate(email, password string) (identityUserInfo, error) {
	email, err := normaliseIdentityEmail(email)
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

// session resolves a raw token to its session and user, dropping expired or disabled entries.
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

	cloned := *session
	info := user.info()
	s.mu.Unlock()

	return cloned, info, nil
}

// deleteSession removes a session by its raw token.
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
