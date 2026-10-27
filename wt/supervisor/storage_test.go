package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad batch: %d, want 400", resp.StatusCode)
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
		`select readfile('/etc/passwd')`,
		`begin`,
		`commit`,
		`rollback`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", statement, resp.StatusCode)
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
