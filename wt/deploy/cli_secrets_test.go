package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSecretsList(t *testing.T) {
	var gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"keys":["OAUTH_CLIENT_SECRET"]}`))
	}))
	defer server.Close()

	if err := runSecrets([]string{"passport", "list", testHostFlag, server.URL, "--token", "secret"}); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/apps/passport/secrets" || gotAuth != "Bearer secret" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
}

func TestRunSecretsSetAndDelete(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth string
		got                         map[string]string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")

		if r.Method == http.MethodPut {
			if err := json.UnmarshalRead(r.Body, &got); err != nil {
				t.Errorf("body: %v", err)
			}
		}

		_, _ = w.Write([]byte(`{"key":"OAUTH_CLIENT_SECRET","restart_required":true}`))
	}))
	defer server.Close()

	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("super-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runSecrets([]string{
		"passport", "set", "OAUTH_CLIENT_SECRET", "--file", file,
		testHostFlag, server.URL, "--token", "secret",
	}); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPut || gotPath != "/apps/passport/secrets/OAUTH_CLIENT_SECRET" || gotAuth != "Bearer secret" {
		t.Errorf("method=%q path=%q auth=%q", gotMethod, gotPath, gotAuth)
	}

	if got["value"] != "super-secret" {
		t.Errorf("value = %q", got["value"])
	}

	if strings.Contains(gotPath, "super-secret") {
		t.Error("secret value leaked into the request path")
	}

	if err := runSecrets([]string{"passport", "delete", "OAUTH_CLIENT_SECRET", testHostFlag, server.URL, "--token", "secret"}); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodDelete || gotPath != "/apps/passport/secrets/OAUTH_CLIENT_SECRET" {
		t.Errorf("delete method=%q path=%q", gotMethod, gotPath)
	}
}

func TestRunSecretsRejectsBadInput(t *testing.T) {
	if err := runSecrets([]string{testHostFlag, "http://127.0.0.1:1", "passport", "set", "bad/key"}); err == nil || !strings.Contains(err.Error(), "invalid secret key") {
		t.Errorf("bad key error = %v", err)
	}

	if err := runSecrets([]string{testHostFlag, "http://127.0.0.1:1", "passport", "delete"}); err == nil {
		t.Error("missing delete key was accepted")
	}

	if err := runSecrets([]string{testHostFlag, "http://127.0.0.1:1", "passport", "explode"}); err == nil || !strings.Contains(err.Error(), "unknown secrets action") {
		t.Errorf("unknown action error = %v", err)
	}
}
