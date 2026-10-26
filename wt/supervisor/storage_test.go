package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func storageRequest(t *testing.T, method, url, token, body string) *http.Response {
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

func TestStorage(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	token := s.tokenFor("notes")
	base := s.url() + "/v1/kv/"

	if resp := storageRequest(t, http.MethodGet, base+"missing", token, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing: %d, want 404", resp.StatusCode)
	}
	if resp := storageRequest(t, http.MethodGet, base+"missing", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", resp.StatusCode)
	}
	if resp := storageRequest(t, http.MethodPut, base+"greeting", token, "hello"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("put: %d, want 204", resp.StatusCode)
	}
	resp := storageRequest(t, http.MethodGet, base+"greeting", token, "")
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "hello" {
		t.Errorf("get: %d, want 200 hello", resp.StatusCode)
	}
	if resp := storageRequest(t, http.MethodGet, s.url()+"/v1/kv", token, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("list: %d, want 200", resp.StatusCode)
	} else if body := readBody(t, resp); !strings.Contains(body, "greeting") {
		t.Errorf("list body = %q", body)
	}
	if resp := storageRequest(t, http.MethodDelete, base+"greeting", token, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d, want 204", resp.StatusCode)
	}
	if resp := storageRequest(t, http.MethodGet, base+"greeting", token, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: %d, want 404", resp.StatusCode)
	}
}

func TestStorageIsolation(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	one := s.tokenFor("one")
	two := s.tokenFor("two")
	url := s.url() + "/v1/kv/shared"

	if resp := storageRequest(t, http.MethodPut, url, one, "one"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if resp := storageRequest(t, http.MethodGet, url, two, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other app saw the key: %d", resp.StatusCode)
	}
}
