package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientFlagsAnywhere(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")

	cfg, rest, err := clientFlags([]string{
		"pulse",
		testHostFlag, "http://127.0.0.1:8080",
		"--token", "secret",
		"--json",
		"--tail", "40",
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://127.0.0.1:8080" || cfg.token != "secret" || !cfg.jsonOut || cfg.timeout != defaultClientTimeout {
		t.Errorf("cfg = %+v", cfg)
	}

	if strings.Join(rest, " ") != "pulse --tail 40" {
		t.Errorf("rest = %v", rest)
	}
}

func TestClientFlagsDoubleDash(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "http://default")

	cfg, rest, err := clientFlags([]string{"--", testHostFlag, "literal", "pulse"}, false)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://default" {
		t.Errorf("host = %q, want the environment default", cfg.host)
	}

	if strings.Join(rest, " ") != "--host literal pulse" {
		t.Errorf("rest = %v", rest)
	}
}

func TestRunLogsAcceptsClientFlagsAfterApp(t *testing.T) {
	var gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"logs":[]}`))
	}))
	defer server.Close()

	if err := runLogs([]string{"pulse", testHostFlag, server.URL, "--token", "secret", "--tail", "40"}); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/apps/pulse/logs?tail=40" || gotAuth != "Bearer secret" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
}

func TestAppURLForHost(t *testing.T) {
	if got := appURLForHost("http://127.0.0.1:8080", "pulse.localhost"); got != "http://pulse.localhost:8080" {
		t.Errorf("local app URL = %q", got)
	}

	if got := appURLForHost("https://wt.example.com", "pulse.example.com"); got != "https://pulse.example.com" {
		t.Errorf("remote app URL = %q", got)
	}
}
