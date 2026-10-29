package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

func storageExec(t *testing.T, s *storageServer, token, sql string, params []any) {
	t.Helper()
	body := map[string]any{"sql": sql}
	if params != nil {
		body["params"] = params
	}
	resp := storageSQL(t, s, token, "/v1/exec", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec %q: %d %s", sql, resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
}

func TestStorageChangeLog(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	storageExec(t, s, token, `create table items (value text not null)`, nil)
	storageExec(t, s, token, `insert into items (value) values (?)`, []any{"one"})
	resp := storageSQL(t, s, token, "/v1/batch", map[string]any{
		"statements": []any{
			map[string]any{"sql": `insert into items (value) values (?)`, "params": []any{"two"}},
			map[string]any{"sql": `insert into items (value) values (?)`, "params": []any{"three"}},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	changes, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 {
		t.Fatalf("changes = %d, want 4: %+v", len(changes), changes)
	}
	if changes[0].SQL != `insert into items (value) values (?)` || changes[0].RowsAffected != 1 {
		t.Errorf("newest change = %+v", changes[0])
	}
	if got := string(changes[2].Params[0]); got != `"one"` {
		t.Errorf("insert params = %s, want %s", got, `"one"`)
	}
	if changes[3].SQL != `create table items (value text not null)` || changes[3].RowsAffected != 0 {
		t.Errorf("oldest change = %+v", changes[3])
	}
	if changes[3].Params == nil {
		t.Error("parameterless change has nil params, want an empty array")
	}
	for _, change := range changes {
		if change.Time.IsZero() {
			t.Errorf("change %q has no timestamp", change.SQL)
		}
	}

	limited, err := s.recentChanges("notes", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].SQL != changes[0].SQL {
		t.Errorf("limited changes = %+v", limited)
	}

	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `insert into missing (value) values ('nope')`,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad exec = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	after, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(changes) {
		t.Errorf("failed exec was logged: %d changes, want %d", len(after), len(changes))
	}
}

func TestStorageRowChanges(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	storageExec(t, s, token, `create table items (id integer primary key, value text not null)`, nil)
	storageExec(t, s, token, `insert into items (id, value) values (?, ?)`, []any{1, "one"})
	storageExec(t, s, token, `update items set value = ? where id = ?`, []any{"two", 1})
	storageExec(t, s, token, `delete from items where id = ?`, []any{1})

	changes, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 {
		t.Fatalf("changes = %d, want 4", len(changes))
	}
	if len(changes[3].Rows) != 0 {
		t.Errorf("create rows = %+v, want none", changes[3].Rows)
	}

	insert := changes[2]
	if len(insert.Rows) != 1 || insert.Rows[0].Op != "insert" || insert.Rows[0].Table != "items" {
		t.Fatalf("insert rows = %+v", insert.Rows)
	}
	insertRow := insert.Rows[0]
	if insertRow.RowID != 1 || len(insertRow.Columns) != 2 || insertRow.Columns[0] != "id" || insertRow.Columns[1] != "value" {
		t.Errorf("insert row = %+v", insertRow)
	}
	if len(insertRow.New) != 2 || insertRow.New[0] != float64(1) || insertRow.New[1] != "one" {
		t.Errorf("insert values = %+v", insertRow.New)
	}

	update := changes[1]
	if len(update.Rows) != 1 || update.Rows[0].Op != "update" {
		t.Fatalf("update rows = %+v", update.Rows)
	}
	updateRow := update.Rows[0]
	if len(updateRow.Old) != 2 || updateRow.Old[1] != "one" || len(updateRow.New) != 2 || updateRow.New[1] != "two" {
		t.Errorf("update values = old %+v new %+v", updateRow.Old, updateRow.New)
	}

	deleteChange := changes[0]
	if len(deleteChange.Rows) != 1 || deleteChange.Rows[0].Op != "delete" {
		t.Fatalf("delete rows = %+v", deleteChange.Rows)
	}
	if len(deleteChange.Rows[0].Old) != 2 || deleteChange.Rows[0].Old[1] != "two" {
		t.Errorf("delete values = %+v", deleteChange.Rows[0].Old)
	}
}

func TestStorageBatchRowChanges(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	storageExec(t, s, token, `create table items (value text not null)`, nil)
	resp := storageSQL(t, s, token, "/v1/batch", map[string]any{
		"statements": []any{
			map[string]any{"sql": `insert into items (value) values (?)`, "params": []any{"one"}},
			map[string]any{"sql": `insert into items (value) values (?)`, "params": []any{"two"}},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	changes, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(changes))
	}
	if len(changes[0].Rows) != 1 || changes[0].Rows[0].New[0] != "two" {
		t.Errorf("second batch rows = %+v", changes[0].Rows)
	}
	if len(changes[1].Rows) != 1 || changes[1].Rows[0].New[0] != "one" {
		t.Errorf("first batch rows = %+v", changes[1].Rows)
	}
}

func TestStorageChangeLogTrimsHugeParams(t *testing.T) {
	change := storageChange{
		SQL:    `insert into items (value) values (?)`,
		Params: []jsontext.Value{jsontext.Value([]byte(`"` + strings.Repeat("x", maxChangeEntrySize) + `"`))},
	}
	trimmed := trimChange(change)
	if len(trimmed.Params) != 1 || !strings.Contains(string(trimmed.Params[0]), "params omitted") {
		t.Errorf("trimmed params = %s", trimmed.Params)
	}
}

func TestHandleDatabaseChanges(t *testing.T) {
	s, storage := testDatabaseSupervisor(t)
	token := storage.tokenFor("notes")
	storageExec(t, storage, token, `create table items (value text not null)`, nil)
	storageExec(t, storage, token, `insert into items (value) values (?)`, []any{"one"})

	rec := databaseGET(t, s, "/apps/notes/database/changes")
	if rec.Code != http.StatusOK {
		t.Fatalf("changes = %d %s", rec.Code, rec.Body.String())
	}
	var result storageChangesResponse
	if err := json.UnmarshalRead(rec.Body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(result.Changes))
	}
	if result.Changes[0].SQL != `insert into items (value) values (?)` || result.Changes[0].RowsAffected != 1 {
		t.Errorf("newest change = %+v", result.Changes[0])
	}
	if len(result.Changes[0].Rows) != 1 || result.Changes[0].Rows[0].New[0] != "one" {
		t.Errorf("newest change rows = %+v", result.Changes[0].Rows)
	}

	for _, target := range []string{
		"/apps/notes/database/changes?limit=0",
		"/apps/notes/database/changes?limit=1001",
		"/apps/notes/database/changes?limit=abc",
	} {
		rec = databaseGET(t, s, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, rec.Code)
		}
	}
}
