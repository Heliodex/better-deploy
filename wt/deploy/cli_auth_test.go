package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func authTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capabilities" {
			http.NotFound(w, r)
			return
		}

		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
}

func writeAuthTestConfig(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), privateFileMode); err != nil {
		t.Fatal(err)
	}
}

func TestAuthLoginSavesCredentialsAndKeepsOtherFields(t *testing.T) {
	server := authTestServer(t, "secret-token")
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeAuthTestConfig(t, path, `{"host":"http://old.example","wasmtime":"/bin/wasmtime","custom":"keep"}`)

	var out, errOut bytes.Buffer

	err := runAuthWithIO(
		[]string{"login", "--host", server.URL, "--config", path, "--token-stdin"},
		strings.NewReader("secret-token\n"),
		&out,
		&errOut,
	)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != server.URL || cfg.Token != "secret-token" || cfg.Wasmtime != "/bin/wasmtime" {
		t.Errorf("saved config = %+v", cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}

	if values["custom"] != "keep" {
		t.Errorf("unknown config field was dropped: %v", values)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if perm := info.Mode().Perm(); perm != privateFileMode {
			t.Errorf("config mode = %04o, want %04o", perm, privateFileMode)
		}
	}

	if strings.Contains(out.String()+errOut.String(), "secret-token") {
		t.Errorf("login output leaked the token: %q %q", out.String(), errOut.String())
	}
}

func TestAuthLoginDoesNotSaveRejectedToken(t *testing.T) {
	server := authTestServer(t, "good-token")
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")

	err := runAuthWithIO(
		[]string{"login", "--host", server.URL, "--config", path, "--token-stdin"},
		strings.NewReader("bad-token\n"),
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected a rejected token error")
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("rejected login wrote %s: %v", path, statErr)
	}
}

func TestAuthLoginRejectsTokenFlag(t *testing.T) {
	server := authTestServer(t, "secret-token")
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")

	err := runAuthWithIO(
		[]string{"login", "--host", server.URL, "--config", path, "--token", "secret-token"},
		strings.NewReader(""),
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected --token to be rejected")
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("rejected login wrote %s: %v", path, statErr)
	}
}

func TestAuthStatusReportsSourcesWithoutToken(t *testing.T) {
	server := authTestServer(t, "secret-token")
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeAuthTestConfig(t, path, `{"host":"`+server.URL+`","token":"secret-token"}`)

	var out, errOut bytes.Buffer
	if err := runAuthWithIO([]string{"status", "--config", path, "--json"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}

	var report authStatusReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.Host != server.URL || report.HostSource != "config file" || report.TokenSource != "config file" {
		t.Errorf("status report = %+v", report)
	}

	if strings.Contains(out.String(), "secret-token") {
		t.Errorf("status output leaked the token: %q", out.String())
	}
}

func TestAuthStatusCheck(t *testing.T) {
	server := authTestServer(t, "secret-token")
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeAuthTestConfig(t, path, `{"host":"`+server.URL+`","token":"secret-token"}`)

	var out, errOut bytes.Buffer
	if err := runAuthWithIO([]string{"status", "--config", path, "--check"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "check: ok") {
		t.Errorf("status did not report a successful check: %q", out.String())
	}
}

func TestAuthLogoutRemovesOnlyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeAuthTestConfig(t, path, `{"host":"https://wt.example.com","token":"secret-token","wasmtime":"/bin/wasmtime"}`)

	var out, errOut bytes.Buffer
	if err := runAuthWithIO([]string{"logout", "--config", path}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Token != "" || cfg.Host != "https://wt.example.com" || cfg.Wasmtime != "/bin/wasmtime" {
		t.Errorf("logout config = %+v", cfg)
	}
}
