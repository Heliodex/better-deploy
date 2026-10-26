package storage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	values := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/kv" {
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"keys":["`+strings.Join(keys, `","`)+`"]}`)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		switch r.Method {
		case http.MethodGet:
			value, ok := values[key]
			if !ok {
				http.NotFound(w, nil)
				return
			}
			_, _ = io.WriteString(w, value)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			values[key] = string(body)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(values, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	t.Setenv("WT_STORAGE_URL", server.URL)
	t.Setenv("WT_STORAGE_TOKEN", "test")
	client, err := New(&http.Client{})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := client.Get("nope"); err != nil || ok {
		t.Fatalf("missing get = %v, %v", ok, err)
	}
	if err := client.Put("greeting", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	value, ok, err := client.Get("greeting")
	if err != nil || !ok || string(value) != "hello" {
		t.Fatalf("get = %q, %v, %v", value, ok, err)
	}
	keys, err := client.List("")
	if err != nil || len(keys) != 1 || keys[0] != "greeting" {
		t.Fatalf("list = %v, %v", keys, err)
	}
	if err := client.Delete("greeting"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := client.Get("greeting"); err != nil || ok {
		t.Fatalf("after delete = %v, %v", ok, err)
	}
}
