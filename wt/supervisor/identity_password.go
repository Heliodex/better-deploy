package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// validateIdentityPassword enforces the password length bounds.
func validateIdentityPassword(password string) error {
	if len(password) < identityPasswordMin || len(password) > identityPasswordMax {
		return errIdentityPassword
	}

	return nil
}

// hashIdentityPassword derives a salted PBKDF2-SHA256 hash in the stored format.
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

// verifyIdentityPassword checks a password against the stored PBKDF2 encoding in constant time.
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

// newIdentityToken returns n random bytes as hex.
func newIdentityToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}

	return hex.EncodeToString(value), nil
}

// identityTokenHash is the SHA-256 hex digest stored for a session token.
func identityTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
