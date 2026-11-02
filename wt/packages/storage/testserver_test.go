//go:build !wasip1

package storage

import (
	"errors"
	"net/http"
	"testing"
)

func TestTestServer(t *testing.T) {
	server, err := NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := server.NewClient(server.Client())
	ctx := t.Context()

	if err := client.Migrate(ctx, 1,
		Statement{SQL: `create table notes (id text primary key, text text not null, created_at text not null)`},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec(ctx, `insert into notes (id, text, created_at) values (?, ?, ?)`, "one", "hello", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var id, text string
	if err := client.QueryRow(ctx, `select id, text from notes where id = ?`, "one").Scan(&id, &text); err != nil {
		t.Fatal(err)
	}
	if id != "one" || text != "hello" {
		t.Errorf("row = %q %q", id, text)
	}

	_, err = client.Exec(ctx, `insert into notes (id, text, created_at) values (?, ?, ?)`, "one", "duplicate", "2026-01-01T00:00:00Z")
	var storageErr *Error
	if !errors.As(err, &storageErr) || storageErr.Status != http.StatusConflict || storageErr.Code != "constraint" {
		t.Errorf("duplicate error = %#v", err)
	}

	if err := client.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(ctx, `insert into notes (id, text, created_at) values (?, ?, ?)`, "two", "world", "2026-01-02T00:00:00Z")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := client.QueryRow(ctx, `select count(*) from notes`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestTestServerRequiresToken(t *testing.T) {
	server, err := NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client := server.NewClient(server.Client())
	client.token = "wrong"
	if _, err := client.Exec(t.Context(), `select 1`); err == nil {
		t.Error("wrong token was accepted")
	}
}
