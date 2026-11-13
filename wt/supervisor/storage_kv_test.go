package main

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKVStoreTTLAndIncr(t *testing.T) {
	store := newKVStore()
	now := time.Unix(1_000, 0)
	store.now = func() time.Time { return now }

	if err := store.set("a", kvRequest{Key: "k", Value: "v", TTLMS: 1_000}); err != nil {
		t.Fatal(err)
	}

	if got := store.get("a", "k"); !got.Found || got.Value != "v" || got.TTLMS != 1_000 {
		t.Errorf("get = %+v", got)
	}

	if got := store.get("b", "k"); got.Found {
		t.Errorf("other app sees key: %+v", got)
	}

	now = now.Add(time.Second)

	if got := store.get("a", "k"); got.Found {
		t.Errorf("expired key = %+v", got)
	}

	for want := range int64(3) {
		resp, err := store.incr("a", kvRequest{Key: "n", TTLMS: 60_000})
		if err != nil || resp.Value != want+1 {
			t.Fatalf("incr %d = %+v, %v", want, resp, err)
		}
	}

	// The window starts when the counter is created, so later increments keep its expiry.
	now = now.Add(30 * time.Second)

	if resp, _ := store.incr("a", kvRequest{Key: "n", TTLMS: 60_000}); resp.Value != 4 || resp.TTLMS != 30_000 {
		t.Errorf("incr in window = %+v", resp)
	}

	now = now.Add(30 * time.Second)

	if resp, _ := store.incr("a", kvRequest{Key: "n", By: 5, TTLMS: 60_000}); resp.Value != 5 {
		t.Errorf("incr after window = %+v, want a fresh counter", resp)
	}

	if err := store.set("a", kvRequest{Key: "text", Value: "x"}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.incr("a", kvRequest{Key: "text"}); err == nil {
		t.Error("incr on text succeeded")
	}

	if !store.remove("a", "text") || store.remove("a", "text") {
		t.Error("remove should report the first delete only")
	}
}

func TestKVStoreBudget(t *testing.T) {
	store := newKVStore()
	now := time.Unix(1_000, 0)
	store.now = func() time.Time { return now }

	value := strings.Repeat("x", maxKVValueBytes-10)
	stored := 0

	for i := range maxKVBytesPerApp/maxKVValueBytes + 2 {
		if err := store.set("a", kvRequest{Key: "k" + strconv.Itoa(i), Value: value, TTLMS: 1_000}); err != nil {
			break
		}

		stored++
	}

	if stored == 0 || stored > maxKVBytesPerApp/(maxKVValueBytes-10) {
		t.Fatalf("stored %d values before the budget", stored)
	}

	if err := store.set("a", kvRequest{Key: "more", Value: value}); err == nil {
		t.Fatal("set over budget succeeded")
	}

	// Expired keys free their budget.
	now = now.Add(2 * time.Second)

	if err := store.set("a", kvRequest{Key: "more", Value: value}); err != nil {
		t.Errorf("set after expiry = %v", err)
	}
}

func TestStorageKVRoutes(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("kvapp")

	resp := storageSQL(t, s, token, "/v1/kv/set", map[string]any{"key": "greeting", "value": "hi", "ttl_ms": 60_000})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set = %d %s", resp.StatusCode, readBody(t, resp))
	}

	_ = resp.Body.Close()

	resp = storageSQL(t, s, token, "/v1/kv/get", map[string]any{"key": "greeting"})

	var got kvResponse
	if err := json.Unmarshal([]byte(readBody(t, resp)), &got); err != nil {
		t.Fatal(err)
	}

	if !got.Found || got.Value != "hi" || got.ExpiresAt == "" {
		t.Errorf("get = %+v", got)
	}

	resp = storageSQL(t, s, token, "/v1/kv/incr", map[string]any{"key": "greeting"})
	if body := readBody(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "not_integer") {
		t.Errorf("incr on text = %d %s", resp.StatusCode, body)
	}

	resp = storageSQL(t, s, token, "/v1/kv/incr", map[string]any{"key": "hits", "ttl_ms": 1_000})
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"value":1`) {
		t.Errorf("incr = %d %s", resp.StatusCode, body)
	}

	resp = storageSQL(t, s, token, "/v1/kv/get", map[string]any{"key": ""})
	if body := readBody(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty key = %d %s", resp.StatusCode, body)
	}

	resp = storageSQL(t, s, token, "/v1/kv/delete", map[string]any{"key": "greeting"})
	if body := readBody(t, resp); !strings.Contains(body, `"deleted":true`) {
		t.Errorf("delete = %s", body)
	}

	s.drop("kvapp")

	if got := s.kv.get("kvapp", "hits"); got.Found {
		t.Errorf("dropped app keeps keys: %+v", got)
	}
}
