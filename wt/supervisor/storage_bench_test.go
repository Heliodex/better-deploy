package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

const benchmarkNotesQuery = `select id, body, created_at from notes order by created_at asc, id asc`

func seedStorageDB(tb testing.TB, db *sql.DB) {
	tb.Helper()
	if _, err := db.Exec(`create table notes (
		id text primary key,
		body text not null,
		created_at text not null
	)`); err != nil {
		tb.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 50 {
		if _, err := tx.Exec(`insert into notes (id, body, created_at) values (?, ?, ?)`,
			fmt.Sprintf("id-%02d", i), fmt.Sprintf("note %d", i), time.Unix(int64(i), 0).UTC()); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func benchmarkStorageServer(b *testing.B) (*storageServer, string) {
	b.Helper()
	s, err := newStorageServer(b.TempDir(), "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.close)
	db, err := s.dbFor("bench")
	if err != nil {
		b.Fatal(err)
	}
	seedStorageDB(b, db)
	return s, s.tokenFor("bench")
}

func benchmarkHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		DisableCompression:  true,
	}
	return &http.Client{Transport: transport}
}

func benchmarkStoragePost(b *testing.B, client *http.Client, url, token, body string) {
	b.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status %d", resp.StatusCode)
	}
}

func BenchmarkStorageQueryHTTP(b *testing.B) {
	s, token := benchmarkStorageServer(b)
	client := benchmarkHTTPClient()
	body := `{"sql":"` + benchmarkNotesQuery + `","max_rows":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkStoragePost(b, client, s.url()+"/v1/query", token, body)
	}
}

func BenchmarkStoragePointQueryHTTP(b *testing.B) {
	s, token := benchmarkStorageServer(b)
	client := benchmarkHTTPClient()
	body := `{"sql":"select id, body, created_at from notes where id = ?","params":["id-00"]}`
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkStoragePost(b, client, s.url()+"/v1/query", token, body)
	}
}

func BenchmarkStorageExecHTTP(b *testing.B) {
	s, token := benchmarkStorageServer(b)
	client := benchmarkHTTPClient()
	body := `{"sql":"insert or replace into notes (id, body, created_at) values ('bench', 'value', '2026-01-01T00:00:00Z')"}`
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkStoragePost(b, client, s.url()+"/v1/exec", token, body)
	}
}

func BenchmarkSQLiteQueryDirect(b *testing.B) {
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "storage.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`pragma journal_mode = WAL`); err != nil {
		b.Fatal(err)
	}
	seedStorageDB(b, db)

	b.ResetTimer()
	for range b.N {
		var id, body, createdAt string
		if err := db.QueryRow(`select id, body, created_at from notes where id = ?`, "id-00").Scan(&id, &body, &createdAt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLiteQueryConfiguredConn(b *testing.B) {
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "storage.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`pragma journal_mode = WAL`); err != nil {
		b.Fatal(err)
	}
	seedStorageDB(b, db)

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	b.ResetTimer()
	for range b.N {
		if err := configureStorageConn(ctx, conn); err != nil {
			b.Fatal(err)
		}
		var id, body, createdAt string
		if err := conn.QueryRowContext(ctx, `select id, body, created_at from notes where id = ?`, "id-00").Scan(&id, &body, &createdAt); err != nil {
			b.Fatal(err)
		}
	}
}
