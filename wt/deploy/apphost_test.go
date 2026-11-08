package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyAppDomain(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		app    string
		domain string
		want   string
	}{
		{"default local host", "notes.localhost", "notes", "example.com", "notes.example.com"},
		{"empty host", "", "notes", "example.com", "notes.example.com"},
		{"custom local prefix", "api.localhost", "notes", "example.com", "api.example.com"},
		{"bare localhost", "localhost", "notes", "example.com", "notes.example.com"},
		{"public host unchanged", "peaks.example.org", "notes", "example.com", "peaks.example.org"},
		{"local domain", "notes.localhost", "notes", "localhost", "notes.localhost"},
		{"no domain", "notes.localhost", "notes", "", "notes.localhost"},
		{"trailing dot domain", "notes.localhost", "notes", "example.com.", "notes.example.com"},
	}

	for _, test := range tests {
		m := manifest{Name: test.app, Host: test.host}
		applyAppDomain(&m, test.domain)

		if m.Host != test.want {
			t.Errorf("%s: host = %q, want %q", test.name, m.Host, test.want)
		}
	}
}

func TestCheckAppHostTarget(t *testing.T) {
	tests := []struct {
		name       string
		supervisor string
		appHost    string
		appName    string
		wantError  bool
	}{
		{"remote public host", "https://wt.example.com", "notes.example.com", "notes", false},
		{"remote localhost host", "https://wt.example.com", "notes.localhost", "notes", true},
		{"remote empty app host", "https://wt.example.com", "", "notes", true},
		{"local loopback supervisor", "http://127.0.0.1:8080", "notes.localhost", "notes", false},
		{"local hostname supervisor", "http://localhost:8080", "notes.localhost", "notes", false},
		{"local ipv6 supervisor", "http://[::1]:8080", "notes.localhost", "notes", false},
		{"remote ipv4 supervisor", "http://192.0.2.10:8080", "notes.localhost", "notes", true},
	}

	for _, test := range tests {
		err := checkAppHostTarget(test.supervisor, test.appHost, test.appName, "remote")
		if (err != nil) != test.wantError {
			t.Errorf("%s: error = %v, wantError %v", test.name, err, test.wantError)
		}

		if err != nil && !strings.Contains(err.Error(), "app_domain") {
			t.Errorf("%s: error does not point at app_domain: %v", test.name, err)
		}
	}
}

func TestPrepareManifestExplicitAppHostWins(t *testing.T) {
	m := manifest{Name: "notes", Host: "notes.localhost"}
	m.Host = "notes.dev.example"

	cfg := &deployConfig{appHost: "notes.dev.example", appDomain: "example.com"}

	if err := prepareManifest(&m, cfg); err != nil {
		t.Fatal(err)
	}

	if m.Host != "notes.dev.example" {
		t.Errorf("host = %q, want the explicit app host", m.Host)
	}
}

func TestPrepareManifestAppliesAppDomain(t *testing.T) {
	m := manifest{Name: "notes", Host: "notes.localhost"}
	cfg := &deployConfig{appDomain: "example.com"}

	if err := prepareManifest(&m, cfg); err != nil {
		t.Fatal(err)
	}

	if m.Host != "notes.example.com" {
		t.Errorf("host = %q, want notes.example.com", m.Host)
	}
}

func TestAppDomainForTarget(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		supervisor string
		domain     string
		want       string
	}{
		{"same remote target", "https://wt.example.com", "https://other.example.com", "example.com", "example.com"},
		{"same local target", "http://127.0.0.1:8080", "http://localhost:8080", "localhost", "localhost"},
		{"remote profile on local host", "https://wt.example.com", "http://127.0.0.1:8080", "example.com", ""},
		{"local profile on remote host", "http://127.0.0.1:8080", "https://wt.example.com", "localhost", ""},
		{"no domain", "https://wt.example.com", "http://127.0.0.1:8080", "", ""},
	}

	for _, test := range tests {
		if got := appDomainForTarget(test.profile, test.supervisor, test.domain); got != test.want {
			t.Errorf("%s: domain = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestDeployRefusesLocalAppHostOnRemoteSupervisor(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "app.json"), `{"name":"notes","host":"notes.localhost"}`)

	err := deploy([]string{"--dir", dir, "--host", "https://wt.example.com", "--config", filepath.Join(dir, "missing.json")})
	if err == nil {
		t.Fatal("deploy accepted a local app host on a remote supervisor")
	}

	if !strings.Contains(err.Error(), "app host") || !strings.Contains(err.Error(), "app_domain") {
		t.Errorf("error = %v, want app host guidance", err)
	}
}
