package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type identityUserFile struct {
	Version int            `json:"version"`
	Users   []identityUser `json:"users"`
}

type identitySessionFile struct {
	Version  int               `json:"version"`
	Sessions []identitySession `json:"sessions"`
}

func (s *identityStore) usersPath() string {
	return filepath.Join(s.dir, identityUsersName)
}

func (s *identityStore) sessionsPath() string {
	return filepath.Join(s.dir, identitySessionsName)
}

// loadUsers loads and validates the user file, rejecting duplicate emails.
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

// loadSessions loads unexpired sessions and drops entries for missing users.
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

// saveUsersLocked writes users atomically in id order; the caller holds the lock.
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

// saveSessionsLocked writes sessions atomically in id order; the caller holds the lock.
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
