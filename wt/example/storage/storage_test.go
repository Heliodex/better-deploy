package storage

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
