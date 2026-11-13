//go:build !wasip1

package storage

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func newHelperClient(t *testing.T) *Client {
	t.Helper()

	server, err := NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)

	return server.NewClient(server.Client())
}

func TestQuoteIdentifier(t *testing.T) {
	if got := QuoteIdentifier(`odd"name`); got != `"odd""name"` {
		t.Errorf("QuoteIdentifier = %s", got)
	}
}

func TestAddColumnIfMissing(t *testing.T) {
	client := newHelperClient(t)
	ctx := t.Context()

	if err := client.Migrate(ctx, 1, Statement{SQL: `create table notes (id text primary key)`}); err != nil {
		t.Fatal(err)
	}

	added, err := client.AddColumnIfMissing(ctx, "notes", "body", `text not null default ''`)
	if err != nil || !added {
		t.Fatalf("first add = %v, %v; want added", added, err)
	}

	added, err = client.AddColumnIfMissing(ctx, "notes", "BODY", `text not null default ''`)
	if err != nil || added {
		t.Errorf("second add = %v, %v; want a no-op", added, err)
	}

	added, err = client.AddColumnIfMissing(ctx, "missing", "body", "text")
	if err != nil || added {
		t.Errorf("missing table = %v, %v; want a no-op", added, err)
	}

	columns, err := client.Columns(ctx, "notes")
	if err != nil || !slices.Equal(columns, []string{"id", "body"}) {
		t.Errorf("columns = %v, %v", columns, err)
	}

	has, err := client.HasColumn(ctx, "notes", "Body")
	if err != nil || !has {
		t.Errorf("HasColumn = %v, %v", has, err)
	}
}

func TestRebuildTable(t *testing.T) {
	client := newHelperClient(t)
	ctx := t.Context()

	if err := client.Migrate(ctx, 1,
		Statement{SQL: `create table answer (id text primary key, question text not null, reviewer text, unique (question))`},
		Statement{SQL: `insert into answer (id, question, reviewer) values ('a', 'q1', null), ('b', 'q2', 'r')`},
	); err != nil {
		t.Fatal(err)
	}

	rebuild := TableRebuild{
		Table:  "answer",
		Create: `create table answer_new (id text primary key, question text not null, reviewer text not null, unique (question, reviewer))`,
		Copy:   `insert into answer_new (id, question, reviewer) select id, question, coalesce(reviewer, 'legacy') from answer`,
		After:  []string{`create index answer_reviewer_idx on answer(reviewer)`},
	}
	if err := client.RebuildTable(ctx, rebuild); err != nil {
		t.Fatal(err)
	}

	definition, ok, err := client.TableSQL(ctx, "answer")
	if err != nil || !ok || !strings.Contains(definition, "unique (question, reviewer)") {
		t.Fatalf("definition = %q %v %v", definition, ok, err)
	}

	var reviewer string
	if err := client.QueryRow(ctx, `select reviewer from answer where id = 'a'`).Scan(&reviewer); err != nil || reviewer != "legacy" {
		t.Errorf("copied reviewer = %q, %v", reviewer, err)
	}

	if _, err := client.Exec(ctx, `insert into answer (id, question, reviewer) values ('c', 'q1', 'other')`); err != nil {
		t.Errorf("new key should allow a second reviewer: %v", err)
	}

	missing := rebuild
	missing.Table = "absent"
	if err := client.RebuildTable(ctx, missing); err != nil {
		t.Errorf("missing table = %v, want a no-op", err)
	}
}

func TestRebuildTableRefusesReferencedTable(t *testing.T) {
	client := newHelperClient(t)
	ctx := t.Context()

	if err := client.Migrate(ctx, 1,
		Statement{SQL: `create table parent (id text primary key)`},
		Statement{SQL: `create table child (id text primary key, parent_id text references parent(id) on delete cascade)`},
	); err != nil {
		t.Fatal(err)
	}

	err := client.RebuildTable(ctx, TableRebuild{
		Table:  "parent",
		Create: `create table parent_new (id text primary key, name text)`,
		Copy:   `insert into parent_new (id) select id from parent`,
	})
	if !errors.Is(err, errRebuildReferenced) || !strings.Contains(err.Error(), "child") {
		t.Errorf("err = %v, want a referenced-table error naming child", err)
	}
}

func TestTestServerRejectsEditedMigration(t *testing.T) {
	client := newHelperClient(t)
	ctx := t.Context()

	first := Statement{SQL: `create table notes (id text primary key)`}
	if err := client.Migrate(ctx, 1, first); err != nil {
		t.Fatal(err)
	}

	if err := client.Migrate(ctx, 1, first); err != nil {
		t.Errorf("unchanged replay = %v, want a no-op", err)
	}

	err := client.Migrate(ctx, 1, first, Statement{SQL: `create table extra (id text primary key)`})

	storageErr, ok := errors.AsType[*Error](err)
	if !ok || storageErr.Status != http.StatusConflict || storageErr.Code != "migration_checksum" || !strings.Contains(storageErr.Message, "version 2") {
		t.Errorf("edited migration = %v, want a migration_checksum conflict pointing at version 2", err)
	}
}
