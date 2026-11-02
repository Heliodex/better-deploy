package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientFlagsReadConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"host":"http://config.example","token":"config-token"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_CONFIG", path)

	cfg, rest, err := clientFlags([]string{"--json"}, true)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://config.example" || cfg.token != "config-token" || !cfg.jsonOut || len(rest) != 0 {
		t.Errorf("cfg = %+v, rest = %v", cfg, rest)
	}

	cfg, _, err = clientFlags([]string{testHostFlag, "http://flag.example", "--token", "flag-token"}, true)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://flag.example" || cfg.token != "flag-token" {
		t.Errorf("flags did not override config: %+v", cfg)
	}
}

func TestAPIRequestSendsBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}

		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	status, _, body, err := apiDo(clientOptions{host: server.URL, token: "secret"}, http.MethodGet, "/apps", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if status != http.StatusOK || string(body) != `[]` {
		t.Errorf("status %d body %q", status, body)
	}
}

func TestCompletionAndHelpDoNotPanic(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		if err := runCompletion([]string{shell}); err != nil {
			t.Errorf("completion %s: %v", shell, err)
		}
	}

	if err := runCompletion([]string{"nope"}); err == nil {
		t.Error("unknown shell was accepted")
	}
}
