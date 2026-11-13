//go:build !wasip1

package storage

import (
	"errors"
	"testing"
	"time"
)

func TestKV(t *testing.T) {
	server, err := NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	now := time.Now()
	server.Now = func() time.Time { return now }
	client := server.NewClient(server.Client())
	ctx := t.Context()

	if _, err := client.KVGet(ctx, "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("missing key = %v, want ErrKeyNotFound", err)
	}

	if err := client.KVSet(ctx, "k", "v", time.Minute); err != nil {
		t.Fatal(err)
	}

	entry, err := client.KVGet(ctx, "k")
	if err != nil || entry.Value != "v" || entry.ExpiresAt.IsZero() {
		t.Errorf("get = %+v, %v", entry, err)
	}

	now = now.Add(time.Minute)

	if _, err := client.KVGet(ctx, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("expired key = %v, want ErrKeyNotFound", err)
	}

	if err := client.KVSet(ctx, "forever", "x", 0); err != nil {
		t.Fatal(err)
	}

	deleted, err := client.KVDelete(ctx, "forever")
	if err != nil || !deleted {
		t.Errorf("delete = %v, %v", deleted, err)
	}

	value, _, err := client.KVIncr(ctx, "n", 2, time.Minute)
	if err != nil || value != 2 {
		t.Errorf("incr = %d, %v", value, err)
	}
}

func TestRateLimit(t *testing.T) {
	server, err := NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	now := time.Now()
	server.Now = func() time.Time { return now }
	client := server.NewClient(server.Client())
	limit := RateLimit{Prefix: "rl:", Limit: 2, Window: time.Minute}

	for i := range 3 {
		decision, err := limit.Allow(t.Context(), client, "user")
		if err != nil {
			t.Fatal(err)
		}

		if want := i < 2; decision.Allowed != want || decision.Count != int64(i+1) {
			t.Errorf("call %d = %+v, want allowed %v", i+1, decision, want)
		}
	}

	if decision, _ := limit.Allow(t.Context(), client, "other"); !decision.Allowed {
		t.Errorf("other key = %+v, want its own window", decision)
	}

	now = now.Add(time.Minute)

	if decision, _ := limit.Allow(t.Context(), client, "user"); !decision.Allowed || decision.Count != 1 {
		t.Errorf("next window = %+v, want a fresh count", decision)
	}
}
