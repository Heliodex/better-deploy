package storage

import (
	"context"
	"errors"
	"time"
)

// KVEntry is one key read from the supervisor's key-value store.
type KVEntry struct {
	Value string
	// ExpiresAt is zero for a key that never expires.
	ExpiresAt time.Time
}

type kvRequest struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
	TTLMS int64  `json:"ttl_ms,omitempty"`
	By    int64  `json:"by,omitempty"`
}

type kvResponse struct {
	Found     bool   `json:"found"`
	Value     string `json:"value,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type kvIncrResponse struct {
	Value     int64  `json:"value"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// ErrKeyNotFound is returned by KVGet for a missing or expired key.
var ErrKeyNotFound = errors.New("storage: key not found")

// kvTTL converts a TTL to whole milliseconds, rounding a positive sub-millisecond TTL up so it still expires.
func kvTTL(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}

	return max(ttl.Milliseconds(), 1)
}

func parseKVExpiry(value string) time.Time {
	if value == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

// KVGet reads a key from the supervisor's per-app key-value store, or returns ErrKeyNotFound.
//
// The store lives in supervisor memory: it is shared by every instance of the app, keeps values across requests and is lost when the supervisor restarts. Use it for caches, counters and short-lived tokens, and SQL for anything that must survive.
func (c *Client) KVGet(ctx context.Context, key string) (KVEntry, error) {
	var resp kvResponse
	if err := c.post(ctx, "/v1/kv/get", kvRequest{Key: key}, &resp); err != nil {
		return KVEntry{}, err
	}

	if !resp.Found {
		return KVEntry{}, ErrKeyNotFound
	}

	return KVEntry{Value: resp.Value, ExpiresAt: parseKVExpiry(resp.ExpiresAt)}, nil
}

// KVSet stores value under key. A ttl of zero or less keeps it until it is deleted or the supervisor restarts.
func (c *Client) KVSet(ctx context.Context, key, value string, ttl time.Duration) error {
	var resp struct{}

	return c.post(ctx, "/v1/kv/set", kvRequest{Key: key, Value: value, TTLMS: kvTTL(ttl)}, &resp)
}

// KVDelete removes key and reports whether a live key was there.
func (c *Client) KVDelete(ctx context.Context, key string) (bool, error) {
	var resp struct {
		Deleted bool `json:"deleted"`
	}

	if err := c.post(ctx, "/v1/kv/delete", kvRequest{Key: key}, &resp); err != nil {
		return false, err
	}

	return resp.Deleted, nil
}

// KVIncr adds by to an integer key and returns the new value and its expiry. A missing key starts at zero with ttl, and later calls keep that expiry, so a counter is a fixed window that resets once it expires.
func (c *Client) KVIncr(ctx context.Context, key string, by int64, ttl time.Duration) (int64, time.Time, error) {
	var resp kvIncrResponse
	if err := c.post(ctx, "/v1/kv/incr", kvRequest{Key: key, By: by, TTLMS: kvTTL(ttl)}, &resp); err != nil {
		return 0, time.Time{}, err
	}

	return resp.Value, parseKVExpiry(resp.ExpiresAt), nil
}

// RateLimit is a fixed-window limit kept in the supervisor's key-value store, so it holds across every instance of the app.
type RateLimit struct {
	// Prefix namespaces the counter keys, such as "rl:login:".
	Prefix string
	// Limit is the number of calls a key may make in one Window.
	Limit  int64
	Window time.Duration
}

// RateDecision is the outcome of one RateLimit.Allow call.
type RateDecision struct {
	Allowed bool
	// Count is the calls made in this window so far, including this one.
	Count, Limit int64
	// Reset is when the window ends; RetryAfter is how long until then.
	Reset      time.Time
	RetryAfter time.Duration
}

// Allow counts one call for key and reports whether it is within the limit.
func (r RateLimit) Allow(ctx context.Context, client *Client, key string) (RateDecision, error) {
	count, reset, err := client.KVIncr(ctx, r.Prefix+key, 1, r.Window)
	if err != nil {
		return RateDecision{}, err
	}

	decision := RateDecision{Allowed: count <= r.Limit, Count: count, Limit: r.Limit, Reset: reset}
	if !reset.IsZero() {
		decision.RetryAfter = max(time.Until(reset), 0)
	}

	return decision, nil
}
