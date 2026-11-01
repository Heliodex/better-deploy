package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func migrateTestTarget(t *testing.T, handler http.Handler) (*supervisor, *app, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	s := testSupervisor()
	s.cfg.storageMigrationTimeout = 500 * time.Millisecond
	a := &app{name: "demo", cfg: appConfig{startupTimeout: time.Second}}
	return s, a, port
}

func TestMigrateAppSuccess(t *testing.T) {
	var calls atomic.Int64
	s, a, port := migrateTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != migratePath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := s.migrateApp(a, port, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestMigrateAppNoEndpoint(t *testing.T) {
	s, a, port := migrateTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	err := s.migrateApp(a, port, make(chan struct{}))
	if !errors.Is(err, errNoMigrateEndpoint) {
		t.Fatalf("error = %v, want errNoMigrateEndpoint", err)
	}
}

func TestMigrateAppFailure(t *testing.T) {
	s, a, port := migrateTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "schema is broken", http.StatusInternalServerError)
	}))
	err := s.migrateApp(a, port, make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "schema is broken") {
		t.Fatalf("error = %v, want the migration failure", err)
	}
}

func TestMigrateAppRetriesUnavailable(t *testing.T) {
	var calls atomic.Int64
	s, a, port := migrateTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "storage is starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := s.migrateApp(a, port, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestMigrateAppOutlivesStartupTimeout(t *testing.T) {
	s, a, port := migrateTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	a.cfg.startupTimeout = 10 * time.Millisecond
	s.cfg.storageMigrationTimeout = 500 * time.Millisecond
	if err := s.migrateApp(a, port, make(chan struct{})); err != nil {
		t.Fatalf("slow migration failed although it was within the storage migration timeout: %v", err)
	}
}
