package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if len(id) != 16 {
			t.Fatalf("id %q has length %d, want 16 hex characters", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]int{"n": 1})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("content-type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", got)
	}
	if !strings.HasSuffix(rec.Body.String(), "\n") {
		t.Error("body should end with a newline")
	}
	var out map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["n"] != 1 {
		t.Errorf("body = %q, %v", rec.Body.String(), err)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "nope")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["error"] != "nope" {
		t.Errorf("body = %q, %v", rec.Body.String(), err)
	}
}

func TestServeStorageUnavailable(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run without storage")
	})
	rec := httptest.NewRecorder()
	ServeStorage(errors.New("down"), nil, next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestServeStorageMigratesOnce(t *testing.T) {
	var migrations atomic.Int64
	migrate := func(context.Context) error {
		migrations.Add(1)
		return nil
	}
	var served atomic.Int64
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := ServeStorage(nil, migrate, next)

	for range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	}
	if got := migrations.Load(); got != 1 {
		t.Errorf("migrations = %d, want 1", got)
	}
	if got := served.Load(); got != 3 {
		t.Errorf("served = %d, want 3", got)
	}
}

func TestServeStorageRetriesFailedMigration(t *testing.T) {
	var attempts atomic.Int64
	migrate := func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("not ready")
		}
		return nil
	}
	handler := ServeStorage(nil, migrate, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first request status = %d, want 500", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("retry status = %d, want 204", rec.Code)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}
