// Helpers for authenticated app-to-app calls through the supervisor.

package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// AppCallerHeader names the verified caller on an internal fetch.
	AppCallerHeader = "X-WT-App-Caller"
	// AppTimestampHeader carries the signature timestamp.
	AppTimestampHeader = "X-WT-App-Timestamp"
	// AppSignatureV2Header carries the HMAC over the caller, method, path, timestamp and body digest.
	AppSignatureV2Header = "X-WT-App-Signature-V2"
	// AppBodyHashHeader carries the SHA-256 of the forwarded body.
	AppBodyHashHeader = "X-WT-App-Body-SHA256"
	// RequestIDHeader carries a request identifier across proxied and outgoing requests.
	RequestIDHeader = "X-Request-Id"

	appFetchURLEnv  = "WT_APP_FETCH_URL"
	appNameEnv      = "WT_APP_NAME"
	appTokenEnv     = "WT_APP_TOKEN"
	appURLEnvPrefix = "WT_APP_URL_"
	appCallMaxSkew  = 5 * time.Minute
	// maxAppCallBody matches the supervisor's app-fetch body cap.
	maxAppCallBody = 8 << 20
)

// AppName returns the current app's name as set by the supervisor.
func AppName() string {
	return os.Getenv(appNameEnv)
}

// AppToken returns the credential used for storage and app-to-app calls.
func AppToken() string {
	if token := os.Getenv(appTokenEnv); token != "" {
		return token
	}
	return os.Getenv("WT_STORAGE_TOKEN")
}

// AppURL returns the internal base URL for another app. Known apps get WT_APP_URL_<NAME>; later apps can use WT_APP_FETCH_URL.
func AppURL(name string) string {
	if value := os.Getenv(appURLEnvName(name)); value != "" {
		return strings.TrimSuffix(value, "/")
	}
	base := strings.TrimSuffix(os.Getenv(appFetchURLEnv), "/")
	if base == "" {
		return ""
	}
	return base + "/apps/" + name
}

// NewAppRequest builds a request to another app, adding its bearer identity. Use it with an HTTP client backed by wasi:http in a component.
func NewAppRequest(ctx context.Context, method, target, path string, body io.Reader) (*http.Request, error) {
	base := AppURL(target)
	if base == "" {
		return nil, fmt.Errorf("no internal URL for app %q", target)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+AppToken())
	return req, nil
}

// Caller verifies the supervisor's signature and returns the calling app. It rejects missing, stale or tampered identity headers.
// The request body is read to verify the v2 body digest and then restored for the handler, so call it before reading the body.
func Caller(r *http.Request) (string, bool) {
	caller := r.Header.Get(AppCallerHeader)
	timestamp := r.Header.Get(AppTimestampHeader)
	v2 := r.Header.Get(AppSignatureV2Header)
	if caller == "" || timestamp == "" || v2 == "" {
		return "", false
	}
	stamp, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "", false
	}
	if skew := time.Since(stamp); skew > appCallMaxSkew || skew < -appCallMaxSkew {
		return "", false
	}
	key := AppToken()
	if key == "" {
		return "", false
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, maxAppCallBody+1))
		if err != nil || len(body) > maxAppCallBody {
			return "", false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	bodyHash := appBodySHA256(body)
	if !hmac.Equal([]byte(r.Header.Get(AppBodyHashHeader)), []byte(bodyHash)) {
		return "", false
	}
	expected := appCallSignatureV2(key, caller, r.Method, r.URL.RequestURI(), timestamp, bodyHash)
	return caller, hmac.Equal([]byte(v2), []byte(expected))
}

func appCallSignatureV2(key, caller, method, path, timestamp, bodyHash string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = io.WriteString(mac, caller+"\n"+method+"\n"+path+"\n"+timestamp+"\n"+bodyHash)
	return hex.EncodeToString(mac.Sum(nil))
}

func appBodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func appURLEnvName(name string) string {
	var key strings.Builder
	key.WriteString(appURLEnvPrefix)
	for _, char := range strings.ToUpper(name) {
		switch {
		case char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			key.WriteRune(char)
		default:
			key.WriteByte('_')
		}
	}
	return key.String()
}
