package main

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func storageDSNValues(t *testing.T, dsn string) url.Values {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if parsed.Scheme != "file" {
		t.Errorf("DSN scheme = %q, want file", parsed.Scheme)
	}
	values, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		t.Fatalf("parse DSN query: %v", err)
	}
	return values
}

func TestStorageDSNSetsHardenedPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.db")
	write := storageDSNValues(t, storageDSN(path, false, 0))
	read := storageDSNValues(t, storageDSN(path, true, 0))

	for name, values := range map[string]url.Values{"write": write, "read": read} {
		if got := values.Get("_busy_timeout"); got != "5000" {
			t.Errorf("%s _busy_timeout = %q, want 5000", name, got)
		}
		if got := values.Get("_foreign_keys"); got != "on" {
			t.Errorf("%s _foreign_keys = %q, want on", name, got)
		}
		if got := values.Get("_synchronous"); got != "NORMAL" {
			t.Errorf("%s _synchronous = %q, want NORMAL", name, got)
		}
		for _, want := range []string{"trusted_schema(OFF)", "temp_store(FILE)", "cache_size(-2048)", "mmap_size(0)"} {
			if !slices.Contains(values["_pragma"], want) {
				t.Errorf("%s DSN is missing pragma %q", name, want)
			}
		}
	}

	if got := write.Get("_journal_mode"); got != "WAL" {
		t.Errorf("write _journal_mode = %q, want WAL", got)
	}
	if got := read.Get("_journal_mode"); got != "" {
		t.Errorf("read _journal_mode = %q, want unset", got)
	}
	if got := write.Get("_txlock"); got != "immediate" {
		t.Errorf("write _txlock = %q, want immediate", got)
	}
	if got := read.Get("_txlock"); got != "" {
		t.Errorf("read _txlock = %q, want unset", got)
	}
	if got := write.Get("_query_only"); got != "" {
		t.Errorf("write _query_only = %q, want unset", got)
	}
	if got := read.Get("_query_only"); got != "1" {
		t.Errorf("read _query_only = %q, want 1", got)
	}
	if slices.Contains(write["_pragma"], "max_page_count(256)") {
		t.Error("write DSN set max_page_count without a quota")
	}

	quota := storageDSNValues(t, storageDSN(path, false, 1<<20))
	if !slices.Contains(quota["_pragma"], "max_page_count(256)") {
		t.Errorf("quota DSN pragmas = %v, want max_page_count(256) for 1 MiB", quota["_pragma"])
	}
	tiny := storageDSNValues(t, storageDSN(path, true, 1))
	if !slices.Contains(tiny["_pragma"], "max_page_count(1)") {
		t.Errorf("tiny quota DSN pragmas = %v, want max_page_count(1)", tiny["_pragma"])
	}
}

func storageConnPragma(t *testing.T, conn *sql.Conn, query string) int64 {
	t.Helper()
	var value int64
	if err := conn.QueryRowContext(context.Background(), query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func TestStoragePoolsApplyPragmasPerConnection(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.setQuota("app", 1<<20)

	db, err := s.dbFor("app")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	for i, conn := range []*sql.Conn{first, second} {
		if got := storageConnPragma(t, conn, "pragma synchronous"); got != 1 {
			t.Errorf("write connection %d synchronous = %d, want 1 (NORMAL)", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma temp_store"); got != 1 {
			t.Errorf("write connection %d temp_store = %d, want 1 (FILE)", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma cache_size"); got != -2048 {
			t.Errorf("write connection %d cache_size = %d, want -2048", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma mmap_size"); got != 0 {
			t.Errorf("write connection %d mmap_size = %d, want 0", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma foreign_keys"); got != 1 {
			t.Errorf("write connection %d foreign_keys = %d, want 1", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma busy_timeout"); got != 5000 {
			t.Errorf("write connection %d busy_timeout = %d, want 5000", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma query_only"); got != 0 {
			t.Errorf("write connection %d query_only = %d, want 0", i+1, got)
		}
		if got := storageConnPragma(t, conn, "pragma max_page_count"); got != 256 {
			t.Errorf("write connection %d max_page_count = %d, want 256", i+1, got)
		}
	}

	var journal string
	if err := first.QueryRowContext(ctx, "pragma journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("write journal_mode = %q, want wal", journal)
	}

	readDB, err := s.readDBFor("app")
	if err != nil {
		t.Fatal(err)
	}
	read, err := readDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()

	if got := storageConnPragma(t, read, "pragma query_only"); got != 1 {
		t.Errorf("read query_only = %d, want 1", got)
	}
	if got := storageConnPragma(t, read, "pragma synchronous"); got != 1 {
		t.Errorf("read synchronous = %d, want 1 (NORMAL)", got)
	}
	if got := storageConnPragma(t, read, "pragma temp_store"); got != 1 {
		t.Errorf("read temp_store = %d, want 1 (FILE)", got)
	}
	if got := storageConnPragma(t, read, "pragma cache_size"); got != -2048 {
		t.Errorf("read cache_size = %d, want -2048", got)
	}
	if got := storageConnPragma(t, read, "pragma mmap_size"); got != 0 {
		t.Errorf("read mmap_size = %d, want 0", got)
	}
	if got := storageConnPragma(t, read, "pragma foreign_keys"); got != 1 {
		t.Errorf("read foreign_keys = %d, want 1", got)
	}
	if got := storageConnPragma(t, read, "pragma busy_timeout"); got != 5000 {
		t.Errorf("read busy_timeout = %d, want 5000", got)
	}
	if got := storageConnPragma(t, read, "pragma max_page_count"); got != 256 {
		t.Errorf("read max_page_count = %d, want 256", got)
	}
}
