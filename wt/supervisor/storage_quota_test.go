package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

func TestStoragePerAppQuotaReopensWhenRaised(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("quota")
	s.setQuota("quota", 1<<20)

	exec := func(sql string, params []any) *http.Response {
		t.Helper()

		body := map[string]any{"sql": sql}
		if params != nil {
			body["params"] = params
		}

		return storageSQL(t, s, token, "/v1/exec", body)
	}

	resp := exec(`create table blobs (id integer primary key, body text not null)`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create table: %d %s", resp.StatusCode, readBody(t, resp))
	}

	resp.Body.Close()

	chunk := strings.Repeat("x", 64<<10)

	var failed *http.Response

	for range 64 {
		resp := exec(`insert into blobs (body) values (?)`, []any{chunk})
		if resp.StatusCode != http.StatusOK {
			failed = resp
			break
		}

		resp.Body.Close()
	}

	if failed == nil {
		t.Fatal("writes never reached the 1 MiB quota")
	}
	defer failed.Body.Close()

	var body storageErrorResponse
	if err := json.UnmarshalRead(failed.Body, &body); err != nil {
		t.Fatal(err)
	}

	if failed.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("over-quota status = %d, want 507: %+v", failed.StatusCode, body)
	}

	if body.Code != "disk_full" {
		t.Errorf("over-quota code = %q, want disk_full", body.Code)
	}

	if body.SQLiteCode == 0 {
		t.Error("over-quota response has no sqlite_code")
	}

	s.setQuota("quota", 0)

	resp = exec(`insert into blobs (body) values (?)`, []any{"after raising the quota"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write after raising quota: %d %s", resp.StatusCode, readBody(t, resp))
	}

	resp.Body.Close()
}
