package storage

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{baseURL: server.URL, token: "test", http: server.Client()}
}

func TestClientQueryScan(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/query" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"columns":["id","count","ok","at","blob"],"rows":[["abc",42,true,"2026-10-26T20:00:00Z",{"$b64":"AAEC"}]]}`)
	})

	rows, err := client.Query(context.Background(), `select * from things`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var (
		id    string
		count int64
		ok    bool
		at    time.Time
		blob  []byte
	)
	if err := rows.Scan(&id, &count, &ok, &at, &blob); err != nil {
		t.Fatal(err)
	}
	if id != "abc" || count != 42 || !ok {
		t.Errorf("scan = %q %d %v", id, count, ok)
	}
	if !at.Equal(time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)) {
		t.Errorf("time = %v", at)
	}
	if string(blob) != "\x00\x01\x02" {
		t.Errorf("blob = %v", blob)
	}
	if rows.Next() {
		t.Error("unexpected second row")
	}
}

func TestClientExecEncodesParams(t *testing.T) {
	var received request
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/exec" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"rows_affected":1,"last_insert_id":7}`)
	})

	result, err := client.Exec(context.Background(), `insert into things (value) values (?)`, []byte{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsAffected != 1 || result.LastInsertID != 7 {
		t.Errorf("result = %+v", result)
	}
	if len(received.Params) != 1 {
		t.Fatalf("params = %v", received.Params)
	}
	blob, ok := received.Params[0].(map[string]any)
	if !ok || blob["$b64"] != "AAEC" {
		t.Errorf("blob param = %v", received.Params[0])
	}
}

func TestClientBatch(t *testing.T) {
	var received batchRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/batch" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"results":[{"rows_affected":1},{"rows_affected":2,"last_insert_id":9}]}`)
	})

	results, err := client.Batch(context.Background(),
		Statement{SQL: `insert into things (value) values (?)`, Args: []any{"one"}},
		Statement{SQL: `insert into things (value) values (?)`, Args: []any{"two"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(received.Statements) != 2 {
		t.Fatalf("statements = %d", len(received.Statements))
	}
	if len(results) != 2 || results[0].RowsAffected != 1 || results[1].LastInsertID != 9 {
		t.Errorf("results = %+v", results)
	}
}

func TestClientQueryTruncated(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"columns":["id"],"rows":[["a"]],"truncated":true}`)
	})
	rows, err := client.Query(context.Background(), `select id from things`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Truncated {
		t.Error("expected truncated rows")
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "a" {
		t.Errorf("id = %q", id)
	}
}

func paramInt(t *testing.T, value any) int {
	t.Helper()
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		t.Fatalf("param %v has unexpected type %T", value, value)
		return 0
	}
}

func TestClientQueryNSendsMaxRows(t *testing.T) {
	var received request
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"columns":["id"],"rows":[["a"]],"truncated":true}`)
	})

	rows, err := client.QueryN(context.Background(), 5, `select id from things`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if received.MaxRows != 5 {
		t.Errorf("max_rows = %d, want 5", received.MaxRows)
	}
	if !rows.Truncated {
		t.Error("expected truncated rows")
	}
	if _, ok := rows.NextPage(); ok {
		t.Error("NextPage advanced rows that were not returned by QueryPage")
	}
}

func TestClientQueryDefaultSendsNoMaxRows(t *testing.T) {
	var received request
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"columns":["id"],"rows":[]}`)
	})

	if _, err := client.Query(context.Background(), `select id from things`); err != nil {
		t.Fatal(err)
	}
	if received.MaxRows != 0 {
		t.Errorf("max_rows = %d, want the server default", received.MaxRows)
	}
	if received.SQL != `select id from things` {
		t.Errorf("sql = %q", received.SQL)
	}
}

func TestClientQueryPageWrapsAndAdvances(t *testing.T) {
	var received request
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"columns":["id"],"rows":[["a"],["b"]]}`)
	})

	rows, err := client.QueryPage(context.Background(), `select id from things;`, Page{Limit: 2, Offset: 4}, "start")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if received.SQL != `select * from (select id from things) limit ? offset ?` {
		t.Errorf("sql = %q", received.SQL)
	}
	if len(received.Params) != 3 {
		t.Fatalf("params = %v", received.Params)
	}
	if received.Params[0] != "start" || paramInt(t, received.Params[1]) != 2 || paramInt(t, received.Params[2]) != 4 {
		t.Errorf("params = %v", received.Params)
	}
	if received.MaxRows != 2 {
		t.Errorf("max_rows = %d, want 2", received.MaxRows)
	}
	if !rows.Truncated {
		t.Error("a full page should be treated as truncated")
	}
	next, ok := rows.NextPage()
	if !ok || next != (Page{Limit: 2, Offset: 6}) {
		t.Errorf("next page = %+v, %v", next, ok)
	}
}

func TestClientQueryPageStopsOnShortPage(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"columns":["id"],"rows":[["a"]]}`)
	})

	rows, err := client.QueryPage(context.Background(), `select id from things`, Page{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Truncated {
		t.Error("a short page should not be truncated")
	}
	if _, ok := rows.NextPage(); ok {
		t.Error("NextPage advanced past the last page")
	}
}

func TestClientQueryPageAdvancesByReturnedRowsWhenServerTruncates(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"columns":["id"],"rows":[["a"]],"truncated":true}`)
	})

	rows, err := client.QueryPage(context.Background(), `select id from things`, Page{Limit: 10, Offset: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if next, ok := rows.NextPage(); !ok || next != (Page{Limit: 10, Offset: 5}) {
		t.Errorf("next page = %+v, %v; want offset 5 based on the returned row", next, ok)
	}
}

func TestClientQueryPageValidatesPage(t *testing.T) {
	client := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("invalid page reached the server")
	})
	if _, err := client.QueryPage(context.Background(), `select 1`, Page{}); err == nil {
		t.Error("zero limit was accepted")
	}
	if _, err := client.QueryPage(context.Background(), `select 1`, Page{Limit: 1, Offset: -1}); err == nil {
		t.Error("negative offset was accepted")
	}
}

func TestClientQueryError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"no such table"}`, http.StatusBadRequest)
	})
	if _, err := client.Query(context.Background(), `select * from missing`); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientQueryRowNoRows(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"columns":["id"],"rows":[]}`)
	})
	var id string
	if err := client.QueryRow(context.Background(), `select id from empty`).Scan(&id); err != ErrNoRows {
		t.Fatalf("error = %v, want ErrNoRows", err)
	}
}

func TestClientSchemaAndMigrate(t *testing.T) {
	var migration migrationRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/schema":
			if r.Method != http.MethodGet {
				t.Errorf("schema method = %s", r.Method)
			}
			_, _ = io.WriteString(w, `{"version":2,"tables":[{"name":"notes","columns":[{"name":"id","type":"text","notNull":false,"primaryKey":1},{"name":"body","type":"text","notNull":true}]}]}`)
		case "/v1/migrate":
			if r.Method != http.MethodPost {
				t.Errorf("migrate method = %s", r.Method)
			}
			if err := json.UnmarshalRead(r.Body, &migration); err != nil {
				t.Errorf("body: %v", err)
			}
			_, _ = io.WriteString(w, `{"version":3,"applied":true}`)
		default:
			t.Errorf("path = %q", r.URL.Path)
		}
	})

	schema, err := client.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if schema.Version != 2 || len(schema.Tables) != 1 || schema.Tables[0].Name != "notes" || len(schema.Tables[0].Columns) != 2 {
		t.Errorf("schema = %+v", schema)
	}
	if schema.Tables[0].Columns[1].Name != "body" || !schema.Tables[0].Columns[1].NotNull || schema.Tables[0].Columns[0].PrimaryKey != 1 {
		t.Errorf("columns = %+v", schema.Tables[0].Columns)
	}

	err = client.Migrate(context.Background(), 3,
		Statement{SQL: `create table things (id text primary key)`},
		Statement{SQL: `insert into things (id) values (?)`, Args: []any{"a"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if migration.Version != 3 || len(migration.Statements) != 2 || migration.Statements[1].SQL != `insert into things (id) values (?)` {
		t.Errorf("migration request = %+v", migration)
	}
}

func TestClientSchemaStats(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/schema" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("stats"); got != "1" {
			t.Errorf("stats = %q, want 1", got)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{
			"version":1,
			"tables":[{
				"name":"notes",
				"columns":[{"name":"id","type":"text","notNull":true,"primaryKey":1}],
				"indexes":[{"name":"notes_body","unique":false,"origin":"c","partial":false,"columns":["body",null]}],
				"foreignKeys":[{"id":0,"seq":0,"table":"users","from":"user_id","to":null,"onUpdate":"NO ACTION","onDelete":"CASCADE","match":"NONE"}],
				"rows":3,
				"bytes":8192
			}],
			"views":[{"name":"note_ids","sql":"CREATE VIEW note_ids AS SELECT id FROM notes","columns":[{"name":"id","type":"text","notNull":false}]}],
			"migrations":[{"version":1,"checksum":"abc","appliedAt":"2026-10-31T00:00:00Z"}]
		}`)
	})

	schema, err := client.SchemaStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if schema.Version != 1 || len(schema.Tables) != 1 || len(schema.Views) != 1 || len(schema.Migrations) != 1 {
		t.Fatalf("schema = %+v", schema)
	}
	table := schema.Tables[0]
	if table.Rows == nil || *table.Rows != 3 || table.Bytes == nil || *table.Bytes != 8192 {
		t.Errorf("stats = rows %v bytes %v", table.Rows, table.Bytes)
	}
	if len(table.Indexes) != 1 || len(table.Indexes[0].Columns) != 2 || table.Indexes[0].Columns[1] != nil {
		t.Errorf("indexes = %+v", table.Indexes)
	}
	if len(table.ForeignKeys) != 1 || table.ForeignKeys[0].To != nil {
		t.Errorf("foreign keys = %+v", table.ForeignKeys)
	}
	if schema.Views[0].Name != "note_ids" || len(schema.Views[0].Columns) != 1 {
		t.Errorf("views = %+v", schema.Views)
	}
	if schema.Migrations[0].Checksum != "abc" {
		t.Errorf("migrations = %+v", schema.Migrations)
	}
}

func TestClientTransaction(t *testing.T) {
	var request transactionRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/transaction" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"results":[{"rows_affected":1,"last_insert_id":3},{"columns":["value"],"rows":[["two"]]}]}`)
	})

	results, err := client.Transaction(context.Background(),
		TransactionStatement{SQL: `insert into items (value) values (?)`, Args: []any{"two"}},
		TransactionStatement{SQL: `select value from items where id = ?`, Args: []any{3}, Query: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Statements) != 2 || request.Statements[0].Query || !request.Statements[1].Query {
		t.Errorf("request = %+v", request)
	}
	if len(results) != 2 || results[0].RowsAffected != 1 || results[0].LastInsertID != 3 {
		t.Errorf("write result = %+v", results)
	}
	if len(results[1].Rows) != 1 || results[1].Rows[0][0] != "two" {
		t.Errorf("read rows = %+v", results[1].Rows)
	}
}

func TestClientWithTxCommit(t *testing.T) {
	var paths []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/v1/sessions":
			_, _ = io.WriteString(w, `{"id":"session-1","expiresAt":"2026-10-31T12:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"rows_affected":1,"last_insert_id":4}`)
		case strings.HasSuffix(r.URL.Path, "/query"):
			_, _ = io.WriteString(w, `{"columns":["value"],"rows":[["two"]]}`)
		case strings.HasSuffix(r.URL.Path, "/commit"):
			_, _ = io.WriteString(w, `{"committed":true}`)
		default:
			t.Errorf("path = %q", r.URL.Path)
		}
	})

	err := client.WithTx(context.Background(), func(tx *Tx) error {
		if tx.ExpiresAt().IsZero() {
			t.Error("session expiry was not decoded")
		}
		result, err := tx.Exec(context.Background(), `insert into items (value) values (?)`, "two")
		if err != nil {
			return err
		}
		if result.RowsAffected != 1 || result.LastInsertID != 4 {
			t.Errorf("exec result = %+v", result)
		}
		rows, err := tx.Query(context.Background(), `select value from items where id = ?`, 4)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("no row")
		}
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		if value != "two" {
			t.Errorf("value = %q", value)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 || paths[0] != "/v1/sessions" || !strings.HasSuffix(paths[1], "/exec") || !strings.HasSuffix(paths[2], "/query") || !strings.HasSuffix(paths[3], "/commit") {
		t.Errorf("paths = %v", paths)
	}
}

func TestClientWithTxRollsBackOnError(t *testing.T) {
	var paths []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/v1/sessions":
			_, _ = io.WriteString(w, `{"id":"session-2","expiresAt":"2026-10-31T12:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"rows_affected":1}`)
		case strings.HasSuffix(r.URL.Path, "/rollback"):
			_, _ = io.WriteString(w, `{"rolled_back":true}`)
		default:
			t.Errorf("path = %q", r.URL.Path)
		}
	})

	sentinel := errors.New("stop")
	err := client.WithTx(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec(context.Background(), `insert into items (value) values (?)`, "three"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want the callback error", err)
	}
	if len(paths) != 3 || !strings.HasSuffix(paths[2], "/rollback") {
		t.Errorf("paths = %v, want a rollback", paths)
	}
}

func TestClientTypedError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"constraint failed: UNIQUE constraint failed: items.id","code":"constraint","sqlite_code":2067,"constraint":"UNIQUE"}`)
	})

	_, err := client.Exec(context.Background(), `insert into items (id) values (?)`, "a")
	if err == nil {
		t.Fatal("expected an error")
	}
	var storageErr *Error
	if !errors.As(err, &storageErr) {
		t.Fatalf("error = %v (%T), want *Error", err, err)
	}
	if storageErr.Status != http.StatusConflict || storageErr.Code != "constraint" || storageErr.SQLiteCode != 2067 || storageErr.Constraint != "UNIQUE" {
		t.Errorf("error = %+v", storageErr)
	}
	if !strings.Contains(storageErr.Message, "UNIQUE constraint failed") {
		t.Errorf("message = %q", storageErr.Message)
	}
}

func TestClientWriteReturning(t *testing.T) {
	var received request
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/write" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.UnmarshalRead(r.Body, &received); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"columns":["id","value"],"rows":[["a","one"]],"rows_affected":1}`)
	})

	rows, err := client.WriteReturning(context.Background(), `insert into items (id, value) values (?, ?) returning id, value`, "a", "one")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if received.SQL != `insert into items (id, value) values (?, ?) returning id, value` || len(received.Params) != 2 {
		t.Errorf("request = %+v", received)
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	var id, value string
	if err := rows.Scan(&id, &value); err != nil {
		t.Fatal(err)
	}
	if id != "a" || value != "one" {
		t.Errorf("row = %q %q", id, value)
	}
}
