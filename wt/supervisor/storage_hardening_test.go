package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageConnectionPragmas(t *testing.T) {
	joined := strings.Join(storageConnectionPragmas, "\n")
	if strings.Contains(joined, "temp_store = MEMORY") {
		t.Error("temp_store must not use MEMORY")
	}
	for _, want := range []string{"temp_store = FILE", "cache_size = -2048", "mmap_size = 0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pragma list is missing %q", want)
		}
	}
}

func TestConfigureStorageConnEnablesHardenedPragmas(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "storage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := configureStorageConn(ctx, conn); err != nil {
		t.Fatal(err)
	}

	read := func(query string) int64 {
		t.Helper()
		var value int64
		if err := conn.QueryRowContext(ctx, query).Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return value
	}
	if got := read("pragma temp_store"); got != 1 {
		t.Errorf("temp_store = %d, want 1 (FILE)", got)
	}
	if got := read("pragma cache_size"); got != -2048 {
		t.Errorf("cache_size = %d, want -2048", got)
	}
	if got := read("pragma mmap_size"); got != 0 {
		t.Errorf("mmap_size = %d, want 0", got)
	}
}
