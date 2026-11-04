// Package sign signs and verifies HMAC-SHA256 webhook payloads.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// DefaultWindow is the accepted clock skew for a timestamped signature.
const DefaultWindow = 5 * time.Minute

var (
	// ErrInvalidTimestamp means the timestamp is not a non-negative Unix second value.
	ErrInvalidTimestamp = errors.New("sign: invalid timestamp")
	// ErrStaleTimestamp means the timestamp is outside the signer's window.
	ErrStaleTimestamp = errors.New("sign: timestamp outside the allowed window")
	// ErrBadSignature means the HMAC did not match.
	ErrBadSignature = errors.New("sign: signature does not match")
)

// Signer computes and verifies timestamped webhook signatures.
//
// The signed value is timestamp + "." + deliveryID + "." + body, where timestamp is Unix seconds.
// Callers that need replay protection must store delivery IDs and reject duplicates.
type Signer struct {
	Secret []byte
	Window time.Duration
	Now    func() time.Time
}

// Sign signs body for deliveryID and returns the timestamp and lowercase hex signature.
func (s Signer) Sign(deliveryID string, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(s.now().Unix(), 10)

	return timestamp, s.SignMessage(timestamp, deliveryID, body)
}

// SignMessage signs an explicit timestamp and delivery ID, which is useful for tests and replays.
func (s Signer) SignMessage(timestamp, deliveryID string, body []byte) string {
	mac := hmac.New(sha256.New, s.Secret)
	_, _ = io.WriteString(mac, timestamp)
	_, _ = io.WriteString(mac, ".")
	_, _ = io.WriteString(mac, deliveryID)
	_, _ = io.WriteString(mac, ".")
	_, _ = mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a timestamped signature and its clock window.
func (s Signer) Verify(timestamp, deliveryID, signature string, body []byte) error {
	stamp := strings.TrimSpace(timestamp)

	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || seconds < 0 {
		return ErrInvalidTimestamp
	}

	now := s.now()
	if skew := now.Sub(time.Unix(seconds, 0)); skew > s.window() || skew < -s.window() {
		return ErrStaleTimestamp
	}

	if !s.VerifyMessage(stamp, deliveryID, signature, body) {
		return ErrBadSignature
	}

	return nil
}

// VerifyMessage checks a signature over an explicit timestamp and delivery ID.
func (s Signer) VerifyMessage(timestamp, deliveryID, signature string, body []byte) bool {
	expected, err := hex.DecodeString(s.SignMessage(timestamp, deliveryID, body))
	if err != nil {
		return false
	}

	actual, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}

	return hmac.Equal(expected, actual)
}

// SignParts signs concatenated parts without separators.
func (s Signer) SignParts(parts ...[]byte) string {
	mac := hmac.New(sha256.New, s.Secret)
	for _, part := range parts {
		_, _ = mac.Write(part)
	}

	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyParts checks a signature over concatenated parts.
func (s Signer) VerifyParts(signature string, parts ...[]byte) bool {
	expected, err := hex.DecodeString(s.SignParts(parts...))
	if err != nil {
		return false
	}

	actual, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}

	return hmac.Equal(expected, actual)
}

// now returns the signer's clock, defaulting to time.Now.
func (s Signer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}

	return time.Now().UTC()
}

// window returns the configured window, or DefaultWindow when it is unset.
func (s Signer) window() time.Duration {
	if s.Window > 0 {
		return s.Window
	}

	return DefaultWindow
}
