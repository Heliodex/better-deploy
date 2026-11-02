package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallerVerifiesBodySignature(t *testing.T) {
	t.Setenv("WT_APP_TOKEN", "key")

	body := []byte(`{"value":"one"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	bodyHash := appBodySHA256(body)

	request := httptest.NewRequest(http.MethodPost, "http://target.localhost/v1/items?limit=1", bytes.NewReader(body))
	request.Header.Set(AppCallerHeader, "caller")
	request.Header.Set(AppTimestampHeader, timestamp)
	request.Header.Set(AppBodyHashHeader, bodyHash)
	request.Header.Set(AppSignatureV2Header, appCallSignatureV2("key", "caller", request.Method, request.URL.RequestURI(), timestamp, bodyHash))

	caller, ok := Caller(request)
	if !ok || caller != "caller" {
		t.Fatalf("Caller = %q, %t", caller, ok)
	}

	restored, _ := io.ReadAll(request.Body)
	if !bytes.Equal(restored, body) {
		t.Errorf("restored body = %q, want %q", restored, body)
	}

	tampered := httptest.NewRequest(http.MethodPost, "http://target.localhost/v1/items?limit=1", bytes.NewReader([]byte(`{"value":"two"}`)))

	tampered.Header = request.Header.Clone()
	if caller, ok := Caller(tampered); ok {
		t.Errorf("tampered body accepted as %q", caller)
	}
}

func TestCallerRejectsUnknownSignature(t *testing.T) {
	t.Setenv("WT_APP_TOKEN", "key")

	body := []byte(`legacy`)
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	request := httptest.NewRequest(http.MethodPost, "http://target.localhost/v1/items", bytes.NewReader(body))
	request.Header.Set(AppCallerHeader, "caller")
	request.Header.Set(AppTimestampHeader, timestamp)
	request.Header.Set("X-WT-App-Signature", "legacy-signature")

	if _, ok := Caller(request); ok {
		t.Error("legacy signature accepted")
	}
}

func TestCallerRejectsStaleAndMissingSignatures(t *testing.T) {
	t.Setenv("WT_APP_TOKEN", "key")

	stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	body := []byte("body")
	bodyHash := appBodySHA256(body)
	request := httptest.NewRequest(http.MethodPost, "http://target.localhost/v1/items", bytes.NewReader(body))
	request.Header.Set(AppCallerHeader, "caller")
	request.Header.Set(AppTimestampHeader, stale)
	request.Header.Set(AppBodyHashHeader, bodyHash)
	request.Header.Set(AppSignatureV2Header, appCallSignatureV2("key", "caller", request.Method, request.URL.RequestURI(), stale, bodyHash))

	if _, ok := Caller(request); ok {
		t.Error("stale signature accepted")
	}

	missing := httptest.NewRequest(http.MethodGet, "http://target.localhost/", nil)
	if _, ok := Caller(missing); ok {
		t.Error("missing signature accepted")
	}
}
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

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
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

	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error != "nope" || out.Code != "invalid" {
		t.Errorf("body = %q, %v", rec.Body.String(), err)
	}
}

func TestErrorCode(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "invalid",
		http.StatusUnauthorized:        "unauthorized",
		http.StatusForbidden:           "forbidden",
		http.StatusNotFound:            "not_found",
		http.StatusConflict:            "conflict",
		http.StatusUnprocessableEntity: "invalid",
		http.StatusTooManyRequests:     "rate_limited",
		http.StatusInternalServerError: "internal",
		http.StatusServiceUnavailable:  "unavailable",
		http.StatusTeapot:              "invalid",
	}
	for status, want := range cases {
		if got := ErrorCode(status); got != want {
			t.Errorf("ErrorCode(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestWriteErrorField(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorField(rec, http.StatusConflict, "conflict", "name", "name is taken")

	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	if out.Code != "conflict" || out.Field != "name" || out.Error != "name is taken" {
		t.Errorf("body = %+v", out)
	}
}

func TestServeStorageUnavailable(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
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

func TestServeStorageHealth(t *testing.T) {
	var migrations atomic.Int64

	migrate := func(context.Context) error {
		migrations.Add(1)
		return nil
	}
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run for the health path")
	})
	handler := ServeStorage(nil, migrate, next)

	for range 2 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	}

	if got := migrations.Load(); got != 1 {
		t.Errorf("migrations = %d, want 1", got)
	}
}

func TestServeStorageHealthUnavailable(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run without storage")
	})
	rec := httptest.NewRecorder()
	ServeStorage(errors.New("down"), nil, next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestServeStorageHealthFailedMigration(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run when migration fails")
	})
	handler := ServeStorage(nil, func(context.Context) error {
		return errors.New("not ready")
	}, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestServeStorageNormalPath(t *testing.T) {
	var served atomic.Int64

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := ServeStorage(nil, func(context.Context) error { return nil }, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	if got := served.Load(); got != 1 {
		t.Errorf("served = %d, want 1", got)
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

func TestServeStorageMigrateEndpoint(t *testing.T) {
	var migrations atomic.Int64

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run for the migrate endpoint")
	})
	handler := ServeStorage(nil, func(context.Context) error {
		migrations.Add(1)
		return nil
	}, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, MigratePath, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("migrate status = %d, want 204", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("health status = %d, want 204", rec.Code)
	}

	if got := migrations.Load(); got != 1 {
		t.Errorf("migrations = %d, want 1", got)
	}
}

func TestServeStorageMigrateEndpointFailureThenRetry(t *testing.T) {
	var attempts atomic.Int64

	handler := ServeStorage(nil, func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("schema is broken")
		}

		return nil
	}, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run for the migrate endpoint")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, MigratePath, nil))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "schema is broken") {
		t.Fatalf("first migrate = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, MigratePath, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("retry status = %d, want 204", rec.Code)
	}

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestServeStorageMigrateEndpointUnavailable(t *testing.T) {
	handler := ServeStorage(errors.New("down"), func(context.Context) error {
		t.Error("migration should not run without storage")
		return nil
	}, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, MigratePath, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestServeStorageMigrateEndpointRequiresPost(t *testing.T) {
	handler := ServeStorage(nil, func(context.Context) error {
		t.Error("migration should not run for a GET")
		return nil
	}, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MigratePath, nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}

	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestServeStorageMigrationSurvivesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	migrate := func(ctx context.Context) error {
		close(started)
		<-release

		return ctx.Err()
	}
	handler := ServeStorage(nil, migrate, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("next should not run for the migrate endpoint")
	}))

	requestContext, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, MigratePath, nil).WithContext(requestContext))
		close(done)
	}()

	<-started
	cancel()
	close(release)
	<-done

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 after the request context was canceled", rec.Code)
	}
}
