package sign

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fixedSigner() Signer {
	return Signer{
		Secret: []byte("secret"),
		Window: 5 * time.Minute,
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func TestSignAndVerify(t *testing.T) {
	signer := fixedSigner()
	body := []byte(`{"ok":true}`)

	timestamp, signature := signer.Sign("delivery-1", body)
	if timestamp != "1700000000" {
		t.Errorf("timestamp = %q", timestamp)
	}

	if err := signer.Verify(timestamp, "delivery-1", signature, body); err != nil {
		t.Errorf("Verify() = %v", err)
	}

	if err := signer.Verify(timestamp, "delivery-2", signature, body); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong delivery error = %v", err)
	}

	if err := signer.Verify(timestamp, "delivery-1", signature, []byte(`{"ok":false}`)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong body error = %v", err)
	}

	if err := signer.Verify(timestamp, "delivery-1", strings.ToUpper(signature), body); err != nil {
		t.Errorf("uppercase signature = %v", err)
	}
}

func TestVerifyRejectsBadInput(t *testing.T) {
	signer := fixedSigner()
	body := []byte("body")
	_, signature := signer.Sign("delivery-1", body)

	for _, test := range []struct {
		name, timestamp, signature string
		want                       error
	}{
		{"missing timestamp", "", signature, ErrInvalidTimestamp},
		{"non-numeric timestamp", "soon", signature, ErrInvalidTimestamp},
		{"negative timestamp", "-1", signature, ErrInvalidTimestamp},
		{"stale timestamp", "1699999000", signature, ErrStaleTimestamp},
		{"future timestamp", "1700001000", signature, ErrStaleTimestamp},
		{"malformed signature", "1700000000", "not-hex", ErrBadSignature},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := signer.Verify(test.timestamp, "delivery-1", test.signature, body); !errors.Is(err, test.want) {
				t.Errorf("Verify() = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSignParts(t *testing.T) {
	signer := fixedSigner()
	parts := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	signature := signer.SignParts(parts...)

	if !signer.VerifyParts(signature, parts...) {
		t.Error("VerifyParts() rejected its own signature")
	}

	if signer.VerifyParts(signature, []byte("abcd")) {
		t.Error("VerifyParts() accepted different parts")
	}
}

func TestDefaultWindow(t *testing.T) {
	signer := Signer{Secret: []byte("secret"), Now: func() time.Time { return time.Unix(1000, 0) }}
	if signer.window() != DefaultWindow {
		t.Errorf("window = %v", signer.window())
	}
}

func TestSignFields(t *testing.T) {
	signer := fixedSigner()
	parts := [][]byte{[]byte("1700000000"), []byte("."), []byte(`{"a":1}`)}
	signature := signer.SignFields(parts...)

	if !signer.VerifyFields(signature, parts...) {
		t.Error("VerifyFields() rejected its own signature")
	}

	if signer.VerifyFields(signature, []byte("1700000000.{\"a\":1}")) {
		t.Error("VerifyFields() accepted the concatenated form")
	}

	if signer.VerifyFields(signature, []byte("a"), []byte("bc")) {
		t.Error("VerifyFields() accepted a different split")
	}

	if signer.VerifyFields(strings.ToUpper(signature)+"zz", parts...) {
		t.Error("VerifyFields() accepted a malformed signature")
	}
}

func TestSignFieldsSeparatesAmbiguousSplits(t *testing.T) {
	signer := fixedSigner()

	if signer.SignParts([]byte("a"), []byte("bc")) != signer.SignParts([]byte("ab"), []byte("c")) {
		t.Fatal("SignParts() no longer has the documented concatenation ambiguity to test against")
	}

	if signer.SignFields([]byte("a"), []byte("bc")) == signer.SignFields([]byte("ab"), []byte("c")) {
		t.Error("SignFields() gives the same signature for different splits")
	}
}
