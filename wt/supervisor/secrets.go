// Per-app write-only secret storage and env reference resolution.

package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	secretDirName       = ".secrets"
	secretRefPrefix     = "secret://"
	maxSecretKeyBytes   = 128
	maxSecretValueBytes = 64 << 10
	maxSecretBodyBytes  = 80 << 10
)

var errSecretInvalid = errors.New("invalid secret")

// secretStore keeps one JSON file per app under WT_STATE_DIR/.secrets. Values are never returned from a listing.
// History retains values seen since startup so rotation and deletion still redact lines already being logged.
type secretStore struct {
	dir string

	mu      sync.Mutex
	current map[string]map[string]string
	history map[string]map[string]struct{}
}

// newSecretStore creates the secret directory when needed and loads every app file. A malformed or unreadable file is an error rather than silently dropping secrets.
func newSecretStore(dir string) (*secretStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("secret store dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secret dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secret dir permissions: %w", err)
	}
	store := &secretStore{
		dir:     dir,
		current: map[string]map[string]string{},
		history: map[string]map[string]struct{}{},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read secret dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasPrefix(name, ".") {
			// An interrupted atomic write leaves a hidden temp file; it is not secret state.
			continue
		}
		app := strings.TrimSuffix(name, ".json")
		if !validName(app) {
			return nil, fmt.Errorf("secret file %q has an invalid app name", name)
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read secrets for %s: %w", app, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secret file permissions for %s: %w", app, err)
		}
		values := map[string]string{}
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("secrets for %s: %w", app, err)
		}
		for key, value := range values {
			if err := validateSecretKey(key); err != nil {
				return nil, fmt.Errorf("secrets for %s: %w", app, err)
			}
			if err := validateSecretValue(value); err != nil {
				return nil, fmt.Errorf("secrets for %s: %w", app, err)
			}
		}
		store.current[app] = values
		store.history[app] = map[string]struct{}{}
		for _, value := range values {
			if value != "" {
				store.history[app][value] = struct{}{}
			}
		}
	}
	return store, nil
}

// validateSecretKey accepts environment-style identifiers with a bounded length.
func validateSecretKey(key string) error {
	if key == "" || len(key) > maxSecretKeyBytes {
		return fmt.Errorf("%w: key must be 1 to %d bytes", errSecretInvalid, maxSecretKeyBytes)
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("%w: key %q is not an environment-style identifier", errSecretInvalid, key)
		}
	}
	return nil
}

// validateSecretValue bounds one value and rejects NUL, which cannot be passed through an environment entry.
func validateSecretValue(value string) error {
	if len(value) > maxSecretValueBytes {
		return fmt.Errorf("%w: value is %d bytes, limit is %d", errSecretInvalid, len(value), maxSecretValueBytes)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%w: value contains NUL", errSecretInvalid)
	}
	return nil
}

func (s *secretStore) file(app string) string {
	return filepath.Join(s.dir, app+".json")
}

// get returns one app's secret value. Listings never use this to expose values.
func (s *secretStore) get(app, key string) (string, bool) {
	if s == nil || !validName(app) || validateSecretKey(key) != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.current[app][key]
	return value, ok
}

// keys lists an app's secret names in sorted order.
func (s *secretStore) keys(app string) []string {
	if s == nil || !validName(app) {
		return []string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedKeys(s.current[app])
}

// values returns a copy of one app's secrets for launch resolution.
func (s *secretStore) values(app string) map[string]string {
	if s == nil || !validName(app) {
		return map[string]string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.current[app]))
	for key, value := range s.current[app] {
		out[key] = value
	}
	return out
}

// set writes one secret atomically and remembers both the new and previous values for redaction.
func (s *secretStore) set(app, key, value string) error {
	if s == nil {
		return fmt.Errorf("secret store unavailable")
	}
	if !validName(app) {
		return fmt.Errorf("%w: app name %q is invalid", errSecretInvalid, app)
	}
	if err := validateSecretKey(key); err != nil {
		return err
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make(map[string]string, len(s.current[app])+1)
	for existing, current := range s.current[app] {
		values[existing] = current
	}
	values[key] = value
	if err := s.writeLocked(app, values); err != nil {
		return err
	}
	s.current[app] = values
	if s.history[app] == nil {
		s.history[app] = map[string]struct{}{}
	}
	if value != "" {
		s.history[app][value] = struct{}{}
	}
	return nil
}

// delete removes one secret and keeps its old value in the redaction history. Deleting an absent key is a no-op.
func (s *secretStore) delete(app, key string) error {
	if s == nil {
		return fmt.Errorf("secret store unavailable")
	}
	if !validName(app) {
		return fmt.Errorf("%w: app name %q is invalid", errSecretInvalid, app)
	}
	if err := validateSecretKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.current[app]
	if !ok {
		return nil
	}
	if _, ok := current[key]; !ok {
		return nil
	}
	values := make(map[string]string, len(current))
	for existing, value := range current {
		if existing != key {
			values[existing] = value
		}
	}
	if err := s.writeLocked(app, values); err != nil {
		return err
	}
	s.current[app] = values
	return nil
}

// remove deletes an app's secret file and redaction history when its state is purged.
func (s *secretStore) remove(app string) error {
	if s == nil {
		return nil
	}
	if !validName(app) {
		return fmt.Errorf("%w: app name %q is invalid", errSecretInvalid, app)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.file(app)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.current, app)
	delete(s.history, app)
	return nil
}

// redactionValues returns every value seen for the app this run, longest first so overlapping secrets redact cleanly.
func (s *secretStore) redactionValues(app string) []string {
	if s == nil || !validName(app) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]string, 0, len(s.history[app]))
	for value := range s.history[app] {
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	return values
}

func (s *secretStore) writeLocked(app string, values map[string]string) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err := writeFileAtomicMode(s.file(app), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write secrets for %s: %w", app, err)
	}
	return nil
}

// appRedactionValues returns stored secret values plus the app's storage token for log redaction.
func (s *supervisor) appRedactionValues(app string) []string {
	values := s.secrets.redactionValues(app)
	if s.storage != nil {
		if token := s.storage.tokenFor(app); token != "" {
			values = append(values, token)
		}
	}
	if len(values) > 1 {
		sort.Slice(values, func(i, j int) bool {
			if len(values[i]) != len(values[j]) {
				return len(values[i]) > len(values[j])
			}
			return values[i] < values[j]
		})
	}
	return values
}

// redactAppText redacts stored secrets and the app's storage token from supervisor-side text such as LastError.
func (s *supervisor) redactAppText(app, text string) string {
	return redactSecrets(text, s.appRedactionValues(app))
}

// redactSecrets replaces every non-empty secret value, longest first, with a fixed marker.
func redactSecrets(text string, values []string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}

type secretKeysResponse struct {
	Keys []string `json:"keys"`
}

type secretMutationResponse struct {
	Key             string `json:"key"`
	RestartRequired bool   `json:"restart_required"`
}

// refreshAppSecrets updates a running app's log redaction after a secret changes.
func (s *supervisor) refreshAppSecrets(name string) {
	if a := s.lookup(name); a != nil && a.logs != nil {
		a.logs.setSecrets(s.appRedactionValues(name))
	}
}

// handleAppSecrets serves the write-only secret API. Listings return names only; writes never echo the value.
func (s *supervisor) handleAppSecrets(w http.ResponseWriter, r *http.Request, name, key string) {
	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}
	if key != "" {
		if err := validateSecretKey(key); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if key == "" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, secretKeysResponse{Keys: s.secrets.keys(name)})
		return
	}

	switch r.Method {
	case http.MethodPut:
		if s.secrets == nil {
			writeError(w, http.StatusServiceUnavailable, "secret store unavailable")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSecretBodyBytes)
		var body struct {
			Value *string `json:"value"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Value == nil {
			writeError(w, http.StatusBadRequest, "value is required")
			return
		}
		if err := s.secrets.set(name, key, *body.Value); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errSecretInvalid) {
				status = http.StatusBadRequest
			}
			writeError(w, status, err.Error())
			return
		}
		s.refreshAppSecrets(name)
		writeJSON(w, http.StatusOK, secretMutationResponse{Key: key, RestartRequired: true})
	case http.MethodDelete:
		if s.secrets == nil {
			writeError(w, http.StatusServiceUnavailable, "secret store unavailable")
			return
		}
		if err := s.secrets.delete(name, key); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errSecretInvalid) {
				status = http.StatusBadRequest
			}
			writeError(w, status, err.Error())
			return
		}
		s.refreshAppSecrets(name)
		writeJSON(w, http.StatusOK, secretMutationResponse{Key: key, RestartRequired: true})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
