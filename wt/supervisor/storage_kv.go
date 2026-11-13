// Ephemeral per-app key-value store with TTLs, served on the storage loopback API.

package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	maxKVKeyBytes    = 512
	maxKVValueBytes  = 64 << 10
	maxKVKeysPerApp  = 10_000
	maxKVBytesPerApp = 8 << 20
	maxKVTTL         = 30 * 24 * time.Hour
	// kvSweepInterval bounds how often a write scans its app's keys for expired entries.
	kvSweepInterval = time.Minute
)

var (
	errKVInvalid = errors.New("invalid kv request")
	errKVFull    = errors.New("kv store is full")
	errKVNotInt  = errors.New("kv value is not an integer")
)

type kvItem struct {
	value   string
	expires time.Time
}

// size is the bytes an item counts against its app's budget.
func (item kvItem) size(key string) int {
	return len(key) + len(item.value)
}

// live reports whether the item has not expired at now. A zero expiry never expires.
func (item kvItem) live(now time.Time) bool {
	return item.expires.IsZero() || now.Before(item.expires)
}

// kvBucket is one app's keys. Expired keys are dropped when read and swept on writes.
type kvBucket struct {
	mu        sync.Mutex
	items     map[string]kvItem
	bytes     int
	lastSweep time.Time
}

func (b *kvBucket) removeLocked(key string) {
	item, ok := b.items[key]
	if !ok {
		return
	}

	b.bytes -= item.size(key)
	delete(b.items, key)
}

func (b *kvBucket) sweepLocked(now time.Time) {
	b.lastSweep = now

	for key, item := range b.items {
		if !item.live(now) {
			b.removeLocked(key)
		}
	}
}

// getLocked returns a live item, dropping it when it has expired.
func (b *kvBucket) getLocked(key string, now time.Time) (kvItem, bool) {
	item, ok := b.items[key]
	if !ok {
		return kvItem{}, false
	}

	if !item.live(now) {
		b.removeLocked(key)
		return kvItem{}, false
	}

	return item, true
}

// putLocked stores item, sweeping expired keys first when the budget would be exceeded.
func (b *kvBucket) putLocked(key string, item kvItem, now time.Time) error {
	if now.Sub(b.lastSweep) >= kvSweepInterval {
		b.sweepLocked(now)
	}

	previous, exists := b.items[key]

	fits := func() bool {
		count, size := len(b.items), b.bytes+item.size(key)
		if exists {
			size -= previous.size(key)
		} else {
			count++
		}

		return count <= maxKVKeysPerApp && size <= maxKVBytesPerApp
	}

	if !fits() {
		b.sweepLocked(now)
		previous, exists = b.items[key]
	}

	if !fits() {
		return fmt.Errorf("%w: limit is %d keys and %d bytes per app", errKVFull, maxKVKeysPerApp, maxKVBytesPerApp)
	}

	if exists {
		b.bytes -= previous.size(key)
	}

	b.items[key] = item
	b.bytes += item.size(key)

	return nil
}

// kvStore holds every app's bucket. It lives in supervisor memory, so it is shared by all of an app's component instances and lost when the supervisor restarts.
type kvStore struct {
	mu      sync.Mutex
	buckets map[string]*kvBucket
	now     func() time.Time
}

func newKVStore() *kvStore {
	return &kvStore{buckets: map[string]*kvBucket{}, now: time.Now}
}

func (s *kvStore) bucket(app string) *kvBucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := s.buckets[app]
	if b == nil {
		b = &kvBucket{items: map[string]kvItem{}}
		s.buckets[app] = b
	}

	return b
}

// drop forgets an app's keys when the app is removed.
func (s *kvStore) drop(app string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	delete(s.buckets, app)
	s.mu.Unlock()
}

func validateKVKey(key string) error {
	if key == "" || len(key) > maxKVKeyBytes {
		return fmt.Errorf("%w: key must be 1 to %d bytes", errKVInvalid, maxKVKeyBytes)
	}

	return nil
}

// kvExpiry turns a TTL in milliseconds into an absolute expiry. Zero means no expiry.
func kvExpiry(now time.Time, ttlMS int64) (time.Time, error) {
	if ttlMS < 0 || ttlMS > maxKVTTL.Milliseconds() {
		return time.Time{}, fmt.Errorf("%w: ttl_ms must be between 0 and %d", errKVInvalid, maxKVTTL.Milliseconds())
	}

	if ttlMS == 0 {
		return time.Time{}, nil
	}

	return now.Add(time.Duration(ttlMS) * time.Millisecond), nil
}

type kvRequest struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
	TTLMS int64  `json:"ttl_ms,omitempty"`
	By    int64  `json:"by,omitempty"`
}

type kvResponse struct {
	Found bool   `json:"found"`
	Value string `json:"value,omitempty"`
	// ExpiresAt is the RFC 3339 expiry, empty for a key that never expires.
	ExpiresAt string `json:"expires_at,omitempty"`
	TTLMS     int64  `json:"ttl_ms,omitempty"`
}

type kvIncrResponse struct {
	Value     int64  `json:"value"`
	ExpiresAt string `json:"expires_at,omitempty"`
	TTLMS     int64  `json:"ttl_ms,omitempty"`
}

// kvTTL describes an item's expiry for a response.
func kvTTL(item kvItem, now time.Time) (string, int64) {
	if item.expires.IsZero() {
		return "", 0
	}

	return item.expires.UTC().Format(time.RFC3339Nano), max(item.expires.Sub(now).Milliseconds(), 1)
}

func (s *kvStore) get(app, key string) kvResponse {
	b := s.bucket(app)
	now := s.now()

	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.getLocked(key, now)
	if !ok {
		return kvResponse{}
	}

	expiresAt, ttl := kvTTL(item, now)

	return kvResponse{Found: true, Value: item.value, ExpiresAt: expiresAt, TTLMS: ttl}
}

func (s *kvStore) set(app string, req kvRequest) error {
	if len(req.Value) > maxKVValueBytes {
		return fmt.Errorf("%w: value is larger than %d bytes", errKVInvalid, maxKVValueBytes)
	}

	now := s.now()

	expires, err := kvExpiry(now, req.TTLMS)
	if err != nil {
		return err
	}

	b := s.bucket(app)

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.putLocked(req.Key, kvItem{value: req.Value, expires: expires}, now)
}

func (s *kvStore) remove(app, key string) bool {
	b := s.bucket(app)
	now := s.now()

	b.mu.Lock()
	defer b.mu.Unlock()

	_, ok := b.getLocked(key, now)
	b.removeLocked(key)

	return ok
}

// incr adds by to an integer key, creating it at zero first. The TTL applies only when the key is created, so a counter is a fixed window that resets when it expires.
func (s *kvStore) incr(app string, req kvRequest) (kvIncrResponse, error) {
	now := s.now()

	expires, err := kvExpiry(now, req.TTLMS)
	if err != nil {
		return kvIncrResponse{}, err
	}

	by := req.By
	if by == 0 {
		by = 1
	}

	b := s.bucket(app)

	b.mu.Lock()
	defer b.mu.Unlock()

	var current int64

	item, ok := b.getLocked(req.Key, now)
	if ok {
		current, err = strconv.ParseInt(item.value, 10, 64)
		if err != nil {
			return kvIncrResponse{}, fmt.Errorf("%w: %q", errKVNotInt, req.Key)
		}

		expires = item.expires
	}

	if (by > 0 && current > math.MaxInt64-by) || (by < 0 && current < math.MinInt64-by) {
		return kvIncrResponse{}, fmt.Errorf("%w: %q would overflow", errKVNotInt, req.Key)
	}

	current += by

	next := kvItem{value: strconv.FormatInt(current, 10), expires: expires}
	if err := b.putLocked(req.Key, next, now); err != nil {
		return kvIncrResponse{}, err
	}

	expiresAt, ttl := kvTTL(next, now)

	return kvIncrResponse{Value: current, ExpiresAt: expiresAt, TTLMS: ttl}, nil
}

func writeKVError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errKVInvalid):
		writeJSON(w, http.StatusBadRequest, storageErrorResponse{Error: err.Error(), Code: "invalid"})
	case errors.Is(err, errKVNotInt):
		writeJSON(w, http.StatusConflict, storageErrorResponse{Error: err.Error(), Code: "not_integer"})
	case errors.Is(err, errKVFull):
		writeJSON(w, http.StatusInsufficientStorage, storageErrorResponse{Error: err.Error(), Code: "kv_full"})
	default:
		writeStorageError(w, err)
	}
}

// readKVRequest decodes a KV request and validates its key.
func readKVRequest(w http.ResponseWriter, r *http.Request) (kvRequest, bool) {
	var req kvRequest
	if !readStorageJSON(w, r, &req) {
		return req, false
	}

	if err := validateKVKey(req.Key); err != nil {
		writeKVError(w, err)
		return req, false
	}

	return req, true
}

// kvHandler serves one /v1/kv operation for the authenticated app.
func (s *storageServer) kvHandler(op string) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, app string) {
		req, ok := readKVRequest(w, r)
		if !ok {
			return
		}

		switch op {
		case "get":
			writeJSON(w, http.StatusOK, s.kv.get(app, req.Key))
		case "set":
			if err := s.kv.set(app, req); err != nil {
				writeKVError(w, err)
				return
			}

			writeJSON(w, http.StatusOK, map[string]bool{"stored": true})
		case "delete":
			writeJSON(w, http.StatusOK, map[string]bool{"deleted": s.kv.remove(app, req.Key)})
		case "incr":
			resp, err := s.kv.incr(app, req)
			if err != nil {
				writeKVError(w, err)
				return
			}

			writeJSON(w, http.StatusOK, resp)
		}
	}
}
