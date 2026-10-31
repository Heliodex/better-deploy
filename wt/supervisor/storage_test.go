package main

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sqlite3 "modernc.org/sqlite/lib"

	"modernc.org/sqlite"
)

func storageDo(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func storageJSONBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func storageSQL(t *testing.T, s *storageServer, token, path string, value any) *http.Response {
	t.Helper()
	return storageDo(t, http.MethodPost, s.url()+path, token, storageJSONBody(t, value))
}

func storageResultOf(t *testing.T, resp *http.Response) storageResult {
	t.Helper()
	defer resp.Body.Close()
	var result storageResult
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStorageSQL(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("notes")
	for _, statement := range []string{
		`create table notes (id text primary key, body text not null, created_at text not null)`,
		`insert into notes (id, body, created_at) values ('a', 'hello', '2026-01-01T00:00:00Z')`,
		`insert into notes (id, body, created_at) values ('b', 'world', '2026-01-02T00:00:00Z')`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec %q: %d %s", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	resp := storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql":      `select id, body, created_at from notes order by created_at`,
		"max_rows": 10,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if strings.Join(result.Columns, ",") != "id,body,created_at" {
		t.Errorf("columns = %v", result.Columns)
	}
	if len(result.Rows) != 2 || result.Rows[0][0] != "a" || result.Rows[1][1] != "world" {
		t.Errorf("rows = %v", result.Rows)
	}
	if result.Truncated {
		t.Error("unexpected truncation")
	}

	resp = storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql":      `select id from notes order by created_at`,
		"max_rows": 1,
	})
	result = storageResultOf(t, resp)
	if len(result.Rows) != 1 || !result.Truncated {
		t.Errorf("limited rows = %v, truncated = %v", result.Rows, result.Truncated)
	}

	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql":    `insert into notes (id, body, created_at) values (?, ?, ?)`,
		"params": []any{"c", "again", "2026-01-03T00:00:00Z"},
	})
	result = storageResultOf(t, resp)
	if result.RowsAffected != 1 {
		t.Errorf("rows affected = %d, want 1", result.RowsAffected)
	}
}

func TestStorageSQLBlob(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("blobs")
	if resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `create table blobs (value blob not null)`,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql":    `insert into blobs (value) values (?)`,
		"params": []any{map[string]string{"$b64": "AAEC"}},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("insert: %d %s", resp.StatusCode, readBody(t, resp))
	}

	resp := storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select value from blobs`,
	})
	result := storageResultOf(t, resp)
	blob, ok := result.Rows[0][0].(map[string]any)
	if !ok || blob["$b64"] != "AAEC" {
		t.Errorf("blob = %#v", result.Rows[0][0])
	}

	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql":    `insert into blobs (value) values (?)`,
		"params": []any{strings.Repeat("x", maxStorageValue+1)},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized value: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStorageSQLBatch(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("batch")
	if resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `create table items (value text not null)`,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}

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

	resp = storageSQL(t, s, token, "/v1/batch", map[string]any{
		"statements": []any{
			map[string]any{"sql": `insert into items (value) values (?)`, "params": []any{"three"}},
			map[string]any{"sql": `insert into missing (value) values (?)`, "params": []any{"four"}},
		},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("bad batch: %d, want 500", resp.StatusCode)
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select count(*) from items`,
	})
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != float64(2) {
		t.Errorf("count = %v, want 2", result.Rows)
	}
}

func TestStorageSQLRejectsUnsafeStatements(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("unsafe")
	for _, statement := range []string{
		`attach database '/tmp/other.db' as other`,
		`detach database other`,
		`vacuum into '/tmp/other.db'`,
		`pragma writable_schema = 1`,
		`select load_extension('/tmp/evil.so')`,
		`select "load_extension"('/tmp/evil.so')`,
		"select `load_extension`('/tmp/evil.so')",
		`select [load_extension]('/tmp/evil.so')`,
		`begin`,
		`commit`,
		`rollback`,
		`savepoint one`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%q: %d, want 403", statement, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestStorageSQLAllowsStringsThatLookUnsafe(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("strings")
	resp := storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select 'load_extension' as text`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != "load_extension" {
		t.Errorf("rows = %v", result.Rows)
	}

	resp = storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select 'begin' as text`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin string: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
}

func TestStorageSQLAppIsolation(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	one := s.tokenFor("one")
	two := s.tokenFor("two")
	if resp := storageSQL(t, s, one, "/v1/exec", map[string]any{
		"sql": `create table private (value text not null)`,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}

	resp := storageSQL(t, s, two, "/v1/query", map[string]any{
		"sql": `select count(*) from sqlite_master where type = 'table' and name = 'private'`,
	})
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != float64(0) {
		t.Errorf("other app saw the table: %v", result.Rows)
	}
}

func TestStorageSchemaAndMigrate(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	migrate := func(version int, statements ...map[string]any) storageMigrationResponse {
		t.Helper()
		resp := storageSQL(t, s, token, "/v1/migrate", map[string]any{"version": version, "statements": statements})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("migrate %d: %d %s", version, resp.StatusCode, readBody(t, resp))
		}
		defer resp.Body.Close()
		var result storageMigrationResponse
		if err := json.UnmarshalRead(resp.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	result := migrate(1, map[string]any{"sql": `create table notes (id text primary key, body text not null)`})
	if !result.Applied || result.Version != 1 {
		t.Errorf("first migration = %+v", result)
	}

	resp := storageDo(t, http.MethodGet, s.url()+"/v1/schema", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("schema: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var schema storageSchema
	if err := json.UnmarshalRead(resp.Body, &schema); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if schema.Version != 1 || len(schema.Tables) != 1 || schema.Tables[0].Name != "notes" || len(schema.Tables[0].Columns) != 2 {
		t.Fatalf("schema = %+v", schema)
	}
	if schema.Tables[0].Columns[0].PrimaryKey != 1 || !schema.Tables[0].Columns[1].NotNull {
		t.Errorf("columns = %+v", schema.Tables[0].Columns)
	}

	// Applying an old version with the same statements is a no-op, so an older process can start against a newer database.
	result = migrate(1, map[string]any{"sql": `create table notes (id text primary key, body text not null)`})
	if result.Applied || result.Version != 1 {
		t.Errorf("repeat migration = %+v, want a no-op", result)
	}

	result = migrate(2, map[string]any{"sql": `alter table notes add column created_at text not null default ''`})
	if !result.Applied || result.Version != 2 {
		t.Errorf("second migration = %+v", result)
	}

	// A failing statement rolls back the whole migration, including the version bump.
	resp = storageSQL(t, s, token, "/v1/migrate", map[string]any{
		"version":    3,
		"statements": []map[string]any{{"sql": `alter table notes add column extra text`}, {"sql": `insert into missing values (1)`}},
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("failed migration reported success")
	}
	resp.Body.Close()
	resp = storageDo(t, http.MethodGet, s.url()+"/v1/schema", token, "")
	if err := json.UnmarshalRead(resp.Body, &schema); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if schema.Version != 2 {
		t.Errorf("version after failed migration = %d, want 2", schema.Version)
	}

	resp = storageSQL(t, s, token, "/v1/migrate", map[string]any{"version": 0})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("version 0 = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Raw SQL still cannot use pragma; migrations are the only way to change user_version.
	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": `pragma user_version = 9`})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("pragma through exec = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStorageTransaction(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	for _, statement := range []string{
		`create table items (id text primary key, value text not null)`,
		`insert into items (id, value) values ('a', 'one')`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec %q: %d %s", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	resp := storageSQL(t, s, token, "/v1/transaction", map[string]any{
		"statements": []map[string]any{
			{"sql": `update items set value = ? where id = ?`, "params": []any{"two", "a"}},
			{"sql": `select value from items where id = ?`, "params": []any{"a"}, "query": true},
			{"sql": `insert into items (id, value) values ('b', 'three')`},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transaction: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var result storageTransactionResponse
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(result.Results) != 3 {
		t.Fatalf("results = %+v", result.Results)
	}
	if result.Results[0].RowsAffected != 1 {
		t.Errorf("update affected = %d, want 1", result.Results[0].RowsAffected)
	}
	if len(result.Results[1].Rows) != 1 || result.Results[1].Rows[0][0] != "two" {
		t.Errorf("select rows = %+v", result.Results[1].Rows)
	}
	if result.Results[2].RowsAffected != 1 {
		t.Errorf("insert affected = %d, want 1", result.Results[2].RowsAffected)
	}

	// A failing statement rolls the earlier writes back.
	resp = storageSQL(t, s, token, "/v1/transaction", map[string]any{
		"statements": []map[string]any{
			{"sql": `insert into items (id, value) values ('c', 'four')`},
			{"sql": `insert into items (id, value) values ('a', 'duplicate')`},
		},
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("failed transaction reported success")
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, "/v1/query", map[string]any{"sql": `select count(*) from items where id = 'c'`})
	resultRows := storageResultOf(t, resp)
	if len(resultRows.Rows) != 1 || resultRows.Rows[0][0].(float64) != 0 {
		t.Errorf("rolled-back row count = %+v", resultRows.Rows)
	}

	resp = storageSQL(t, s, token, "/v1/transaction", map[string]any{"statements": []map[string]any{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty transaction = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func storageSessionOf(t *testing.T, s *storageServer, token string) storageSessionResponse {
	t.Helper()
	resp := storageSQL(t, s, token, "/v1/sessions", map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var session storageSessionResponse
	if err := json.UnmarshalRead(resp.Body, &session); err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.ExpiresAt.IsZero() {
		t.Fatalf("session = %+v", session)
	}
	return session
}

func storageSessionCount(t *testing.T, s *storageServer, token, table string) float64 {
	t.Helper()
	resp := storageSQL(t, s, token, "/v1/query", map[string]any{"sql": `select count(*) from ` + table})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count %s: %d %s", table, resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		t.Fatalf("count rows = %+v", result.Rows)
	}
	return result.Rows[0][0].(float64)
}

func TestStorageSessionCommit(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": `create table items (id text primary key, value text not null)`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create table: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	session := storageSessionOf(t, s, token)
	base := "/v1/sessions/" + session.ID
	resp = storageSQL(t, s, token, base+"/exec", map[string]any{
		"sql":    `insert into items (id, value) values (?, ?)`,
		"params": []any{"a", "one"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session exec: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, base+"/query", map[string]any{"sql": `select value from items where id = 'a'`})
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != "one" {
		t.Errorf("uncommitted session read = %+v", result.Rows)
	}
	if count := storageSessionCount(t, s, token, "items"); count != 0 {
		t.Errorf("external count before commit = %v, want 0", count)
	}

	resp = storageSQL(t, s, token, base+"/commit", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session commit: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if count := storageSessionCount(t, s, token, "items"); count != 1 {
		t.Errorf("count after commit = %v, want 1", count)
	}
	changes, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	for _, change := range changes {
		if strings.Contains(change.SQL, "insert into items") {
			committed = true
		}
	}
	if !committed {
		t.Error("committed session write was not added to the change log")
	}
}

func TestStorageSessionRollback(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": `create table items (id text primary key, value text not null)`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create table: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	session := storageSessionOf(t, s, token)
	base := "/v1/sessions/" + session.ID
	resp = storageSQL(t, s, token, base+"/exec", map[string]any{
		"sql":    `insert into items (id, value) values (?, ?)`,
		"params": []any{"b", "two"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session exec: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, base+"/rollback", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session rollback: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if count := storageSessionCount(t, s, token, "items"); count != 0 {
		t.Errorf("count after rollback = %v, want 0", count)
	}
	changes, err := s.recentChanges("notes", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if strings.Contains(change.SQL, "insert into items") {
			t.Error("rolled-back session write was added to the change log")
		}
	}
}

func TestStorageSessionLeaseExpiryRollsBack(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": `create table items (id text primary key, value text not null)`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create table: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	session := storageSessionOf(t, s, token)
	base := "/v1/sessions/" + session.ID
	resp = storageSQL(t, s, token, base+"/exec", map[string]any{
		"sql":    `insert into items (id, value) values (?, ?)`,
		"params": []any{"c", "three"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session exec: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	s.sessionMu.Lock()
	s.sessions[session.ID].expiresAt = time.Now().Add(-time.Second)
	s.sessionMu.Unlock()
	s.expireSessions(time.Now())

	if count := storageSessionCount(t, s, token, "items"); count != 0 {
		t.Errorf("count after lease expiry = %v, want 0", count)
	}
	resp = storageSQL(t, s, token, base+"/query", map[string]any{"sql": `select count(*) from items`})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expired session query = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStorageSessionIsScopedToApp(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("notes")
	other := s.tokenFor("other")
	session := storageSessionOf(t, s, token)

	resp := storageSQL(t, s, other, "/v1/sessions/"+session.ID+"/query", map[string]any{"sql": `select 1`})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("other app used session: %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// storageErrorOf decodes the flat, typed error body.
func storageErrorOf(t *testing.T, resp *http.Response) storageErrorResponse {
	t.Helper()
	defer resp.Body.Close()
	var body storageErrorResponse
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestStorageQueryIsReadOnly(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("readonly")

	for _, statement := range []string{
		`create table items (id text primary key, value text not null)`,
		`insert into items (id, value) values ('a', 'one')`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec %q: %d %s", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	for _, statement := range []string{
		`insert into items (id, value) values ('b', 'two')`,
		`insert into items (id, value) values ('b', 'two') returning id`,
		`update items set value = 'changed'`,
		`delete from items`,
		`create table extra (id integer)`,
		`alter table items add column extra text`,
		`drop table items`,
		`pragma user_version`,
		`begin`,
	} {
		resp := storageSQL(t, s, token, "/v1/query", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("query %q: %d, want 403", statement, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		body := storageErrorOf(t, resp)
		if body.Code != "denied" || body.Error == "" {
			t.Errorf("query %q error = %+v", statement, body)
		}
	}

	for _, statement := range []string{
		`select id, value from items`,
		`with counted as (select count(*) as n from items) select n from counted`,
		`explain select * from items`,
		`select * from pragma_table_info('items')`,
		`select 'attach', 'pragma', 'load_extension'`,
	} {
		resp := storageSQL(t, s, token, "/v1/query", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("query %q: %d, want 200: %s", statement, resp.StatusCode, readBody(t, resp))
			continue
		}
		resp.Body.Close()
	}
}

func TestStorageQueryReadsFTS(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("fts")

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `create virtual table docs using fts5(body)`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Skipf("fts5 is unavailable: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `insert into docs (body) values ('hello storage')`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insert: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select body from docs where docs match 'hello'`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fts query: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != "hello storage" {
		t.Errorf("fts rows = %v", result.Rows)
	}
}

func TestStorageWriteReturning(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("returning")

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `create table items (id text primary key, value text not null)`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	resp = storageSQL(t, s, token, "/v1/write", map[string]any{
		"sql":    `insert into items (id, value) values (?, ?) returning id, value`,
		"params": []any{"a", "one"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if strings.Join(result.Columns, ",") != "id,value" || len(result.Rows) != 1 || result.Rows[0][0] != "a" || result.Rows[0][1] != "one" {
		t.Fatalf("write result = %+v", result)
	}
	if result.RowsAffected != 1 {
		t.Errorf("rows affected = %d, want 1", result.RowsAffected)
	}

	changes, err := s.recentChanges("returning", 10)
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, change := range changes {
		if change.SQL == `insert into items (id, value) values (?, ?) returning id, value` {
			if change.RowsAffected != 1 || len(change.Rows) != 1 {
				t.Errorf("logged change = %+v", change)
			}
			logged = true
		}
	}
	if !logged {
		t.Error("returning write was not added to the change log")
	}

	resp = storageSQL(t, s, token, "/v1/write", map[string]any{
		"sql": `update items set value = 'two' where id = 'a' returning value`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update returning: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result = storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != "two" || result.RowsAffected != 1 {
		t.Errorf("update returning = %+v", result)
	}
}

func TestStorageAllowsUnsafeLookingIdentifiers(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("identifiers")

	for _, statement := range []string{
		`create table "pragma" ("attach" text not null, "load_extension" text)`,
		`insert into "pragma" ("attach", "load_extension") values ('kept', 'also kept')`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec %q: %d %s", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	resp := storageSQL(t, s, token, "/v1/query", map[string]any{
		"sql": `select "attach", "load_extension" from "pragma"`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 || result.Rows[0][0] != "kept" || result.Rows[0][1] != "also kept" {
		t.Errorf("rows = %v", result.Rows)
	}
}

func TestStorageTypedConstraintError(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("constraints")

	for _, statement := range []string{
		`create table items (id text primary key, value text unique)`,
		`insert into items (id, value) values ('a', 'one')`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec %q: %d %s", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	resp := storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `insert into items (id, value) values ('a', 'duplicate')`,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate insert = %d, want 409: %s", resp.StatusCode, readBody(t, resp))
	}
	body := storageErrorOf(t, resp)
	if body.Code != "constraint" || body.SQLiteCode == 0 || body.Constraint == "" || body.Error == "" {
		t.Errorf("constraint error = %+v", body)
	}
}

func TestStorageUnknownSQLIsInternal(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("unknown")

	resp := storageSQL(t, s, token, "/v1/query", map[string]any{"sql": `select * from missing`})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("missing table = %d, want 500", resp.StatusCode)
	}
	body := storageErrorOf(t, resp)
	if body.Code != "internal" {
		t.Errorf("error = %+v", body)
	}
}

func TestStorageSQLiteErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code             int
		status           int
		name, constraint string
	}{
		{sqlite3.SQLITE_CONSTRAINT_CHECK, http.StatusConflict, "constraint", "CHECK"},
		{sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, http.StatusConflict, "constraint", "FOREIGN KEY"},
		{sqlite3.SQLITE_CONSTRAINT_NOTNULL, http.StatusConflict, "constraint", "NOT NULL"},
		{sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, http.StatusConflict, "constraint", "PRIMARY KEY"},
		{sqlite3.SQLITE_CONSTRAINT_TRIGGER, http.StatusConflict, "constraint", "TRIGGER"},
		{sqlite3.SQLITE_CONSTRAINT_UNIQUE, http.StatusConflict, "constraint", "UNIQUE"},
		{sqlite3.SQLITE_BUSY, http.StatusServiceUnavailable, "busy", ""},
		{sqlite3.SQLITE_LOCKED, http.StatusServiceUnavailable, "busy", ""},
		{sqlite3.SQLITE_FULL, http.StatusInsufficientStorage, "disk_full", ""},
		{sqlite3.SQLITE_READONLY, http.StatusForbidden, "readonly", ""},
		{sqlite3.SQLITE_AUTH, http.StatusForbidden, "denied", ""},
		{sqlite3.SQLITE_PERM, http.StatusForbidden, "denied", ""},
		{sqlite3.SQLITE_TOOBIG, http.StatusBadRequest, "invalid", ""},
		{sqlite3.SQLITE_RANGE, http.StatusBadRequest, "invalid", ""},
		{sqlite3.SQLITE_MISMATCH, http.StatusBadRequest, "invalid", ""},
		{sqlite3.SQLITE_ERROR, http.StatusInternalServerError, "internal", ""},
	} {
		status, name, constraint := storageSQLiteError(tc.code)
		if status != tc.status || name != tc.name || constraint != tc.constraint {
			t.Errorf("code %d = (%d, %q, %q), want (%d, %q, %q)", tc.code, status, name, constraint, tc.status, tc.name, tc.constraint)
		}
	}
}

func TestStorageAuthorizerPolicy(t *testing.T) {
	write := storageAuthorizer(false)
	for _, action := range []sqlite.AuthorizerActionCode{
		sqlite.AuthAttach, sqlite.AuthDetach, sqlite.AuthPragma, sqlite.AuthTransaction, sqlite.AuthSavepoint,
	} {
		if got := write(action, "", "", "", ""); got != sqlite.AuthorizerDeny {
			t.Errorf("write action %d = %d, want deny", action, got)
		}
	}
	if got := write(sqlite.AuthFunction, "", "LOAD_EXTENSION", "", ""); got != sqlite.AuthorizerDeny {
		t.Errorf("load_extension = %d, want deny", got)
	}
	for _, name := range []string{"readfile", "writefile", "edit"} {
		if got := write(sqlite.AuthFunction, "", name, "", ""); got != sqlite.AuthorizerDeny {
			t.Errorf("%s = %d, want deny", name, got)
		}
	}
	if got := write(sqlite.AuthCreateVTable, "docs", "fts5", "", ""); got != sqlite.AuthorizerOK {
		t.Errorf("fts5 create = %d, want allow", got)
	}
	if got := write(sqlite.AuthCreateVTable, "evil", "evil_module", "", ""); got != sqlite.AuthorizerDeny {
		t.Errorf("unknown vtable = %d, want deny", got)
	}
	for _, action := range []sqlite.AuthorizerActionCode{sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete, sqlite.AuthCreateTable} {
		if got := write(action, "", "", "", ""); got != sqlite.AuthorizerOK {
			t.Errorf("write action %d = %d, want allow", action, got)
		}
	}

	read := storageAuthorizer(true)
	for _, action := range []sqlite.AuthorizerActionCode{
		sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete,
		sqlite.AuthCreateTable, sqlite.AuthCreateIndex, sqlite.AuthCreateTrigger, sqlite.AuthCreateView,
		sqlite.AuthCreateVTable, sqlite.AuthDropTable, sqlite.AuthAlterTable, sqlite.AuthReindex, sqlite.AuthAnalyze,
	} {
		if got := read(action, "", "", "", ""); got != sqlite.AuthorizerDeny {
			t.Errorf("read action %d = %d, want deny", action, got)
		}
	}
	for _, action := range []sqlite.AuthorizerActionCode{sqlite.AuthRead, sqlite.AuthSelect, sqlite.AuthRecursive} {
		if got := read(action, "", "", "", ""); got != sqlite.AuthorizerOK {
			t.Errorf("read action %d = %d, want allow", action, got)
		}
	}
	// A table-valued pragma function is allowed, but a real writable PRAGMA statement is not, even after a pragma_ read.
	if got := read(sqlite.AuthRead, "pragma_table_info", "", "", ""); got != sqlite.AuthorizerOK {
		t.Errorf("pragma_table_info read = %d, want allow", got)
	}
	if got := read(sqlite.AuthPragma, "table_info", "t", "", ""); got != sqlite.AuthorizerOK {
		t.Errorf("table_info function = %d, want allow", got)
	}
	if got := read(sqlite.AuthRead, "pragma_user_version", "", "", ""); got != sqlite.AuthorizerOK {
		t.Errorf("pragma_user_version read = %d, want allow", got)
	}
	if got := read(sqlite.AuthPragma, "writable_schema", "1", "", ""); got != sqlite.AuthorizerDeny {
		t.Errorf("writable_schema = %d, want deny", got)
	}
	if got := read(sqlite.AuthPragma, "data_version", "", "", ""); got != sqlite.AuthorizerOK {
		t.Errorf("data_version = %d, want allow", got)
	}
}

func TestStorageAuthorizerFailsClosed(t *testing.T) {
	if err := registerStorageAuthorizer(struct{}{}, nil); !errors.Is(err, errStorageAuthorizerUnsupported) {
		t.Fatalf("unsupported driver error = %v, want %v", err, errStorageAuthorizerUnsupported)
	}
}
