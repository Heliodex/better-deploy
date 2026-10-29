package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testDatabaseSupervisor(t *testing.T) (*supervisor, *storageServer) {
	t.Helper()
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.close)
	s := testSupervisor()
	s.storage = storage
	s.apps["notes"] = &app{name: "notes"}
	s.apps["empty"] = &app{name: "empty"}
	return s, storage
}

func databaseGET(t *testing.T, s *supervisor, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleApp(rec, req)
	return rec
}

func TestDatabaseOverview(t *testing.T) {
	s, storage := testDatabaseSupervisor(t)
	token := storage.tokenFor("notes")

	rec := databaseGET(t, s, "/apps/notes/database")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty overview = %d %s", rec.Code, rec.Body.String())
	}
	var empty databaseOverviewResponse
	if err := json.UnmarshalRead(rec.Body, &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Database || len(empty.Tables) != 0 {
		t.Errorf("empty overview = %+v", empty)
	}

	createItems(t, storage, token)
	rec = databaseGET(t, s, "/apps/notes/database")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}
	var overview databaseOverviewResponse
	if err := json.UnmarshalRead(rec.Body, &overview); err != nil {
		t.Fatal(err)
	}
	if !overview.Database || len(overview.Tables) != 1 || overview.Tables[0] != "items" {
		t.Errorf("overview = %+v", overview)
	}

	rec = databaseGET(t, s, "/apps/empty/database")
	if rec.Code != http.StatusOK {
		t.Fatalf("no database = %d %s", rec.Code, rec.Body.String())
	}
	var none databaseOverviewResponse
	if err := json.UnmarshalRead(rec.Body, &none); err != nil {
		t.Fatal(err)
	}
	if none.Database || len(none.Tables) != 0 {
		t.Errorf("no database overview = %+v", none)
	}
}

func TestDatabaseRows(t *testing.T) {
	s, storage := testDatabaseSupervisor(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	insertItem(t, storage, token, "two")
	insertItem(t, storage, token, "three")

	resp := storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql":    `create table mixed (id integer primary key, text_value text, blob_value blob)`,
		"params": []any{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create mixed: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	resp = storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql":    `insert into mixed (text_value, blob_value) values (?, ?)`,
		"params": []any{"hello", map[string]string{"$b64": "AAEC"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insert mixed: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	rec := databaseGET(t, s, "/apps/notes/database/rows?table=items&limit=2&offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("rows = %d %s", rec.Code, rec.Body.String())
	}
	var page databaseRowsResponse
	if err := json.UnmarshalRead(rec.Body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Table != "items" || page.Total != 3 || len(page.Rows) != 2 || !page.Truncated {
		t.Errorf("first page = %+v", page)
	}
	if len(page.Columns) != 1 || page.Columns[0] != "value" {
		t.Errorf("columns = %v", page.Columns)
	}

	rec = databaseGET(t, s, "/apps/notes/database/rows?table=items&limit=2&offset=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("last page = %d %s", rec.Code, rec.Body.String())
	}
	if err := json.UnmarshalRead(rec.Body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Truncated {
		t.Errorf("last page = %+v", page)
	}

	rec = databaseGET(t, s, "/apps/notes/database/rows?table=mixed")
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed = %d %s", rec.Code, rec.Body.String())
	}
	if err := json.UnmarshalRead(rec.Body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || len(page.Rows[0]) != 3 {
		t.Fatalf("mixed rows = %+v", page.Rows)
	}
	if page.Rows[0][1] != "hello" {
		t.Errorf("text value = %#v", page.Rows[0][1])
	}
	blob, ok := page.Rows[0][2].(map[string]any)
	if !ok || blob["$b64"] != "AAEC" {
		t.Errorf("blob value = %#v", page.Rows[0][2])
	}
}

func TestDatabaseRowsValidation(t *testing.T) {
	s, storage := testDatabaseSupervisor(t)
	createItems(t, storage, storage.tokenFor("notes"))

	cases := []struct {
		target string
		status int
	}{
		{"/apps/notes/database/rows", http.StatusBadRequest},
		{"/apps/notes/database/rows?table=missing", http.StatusNotFound},
		{`/apps/notes/database/rows?table=items%22%3B%20drop%20table%20items`, http.StatusNotFound},
		{"/apps/notes/database/rows?table=items&limit=0", http.StatusBadRequest},
		{"/apps/notes/database/rows?table=items&limit=501", http.StatusBadRequest},
		{"/apps/notes/database/rows?table=items&offset=-1", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := databaseGET(t, s, tc.target)
		if rec.Code != tc.status {
			t.Errorf("%s = %d %s, want %d", tc.target, rec.Code, rec.Body.String(), tc.status)
		}
	}
}
