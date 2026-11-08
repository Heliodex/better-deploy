package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const profileTestConfig = `{
  "host": "https://legacy.example",
  "token": "legacy-token",
  "profile": "remote",
  "profiles": {
    "local": {
      "host": "http://127.0.0.1:8080",
      "token": "local-token",
      "app_domain": "localhost"
    },
    "remote": {
      "host": "https://wt.example.com",
      "token": "remote-token",
      "app_domain": "example.com"
    }
  }
}`

func writeProfileTestConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	writeTestFile(t, path, profileTestConfig)

	return path
}

func TestClientFlagsSelectsProfile(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, rest, err := clientFlags([]string{"apps", "--config", path, "--profile", "local"}, false)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://127.0.0.1:8080" || cfg.token != "local-token" || cfg.profile != "local" || cfg.appDomain != "localhost" {
		t.Errorf("cfg = %+v", cfg)
	}

	if strings.Join(rest, " ") != "apps" {
		t.Errorf("rest = %v", rest)
	}
}

func TestClientFlagsUsesDefaultProfile(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, _, err := clientFlags([]string{"--config", path}, false)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "https://wt.example.com" || cfg.token != "remote-token" || cfg.profile != "remote" || cfg.appDomain != "example.com" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestClientFlagsProfileFromEnvironment(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "local")

	path := writeProfileTestConfig(t)

	cfg, _, err := clientFlags([]string{"--config", path}, false)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://127.0.0.1:8080" || cfg.token != "local-token" || cfg.profile != "local" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestClientFlagsRejectsUnknownProfile(t *testing.T) {
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	_, _, err := clientFlags([]string{"--config", path, "--profile", "staging"}, false)
	if err == nil {
		t.Fatal("unknown profile was accepted")
	}

	for _, fragment := range []string{"staging", "local", "remote"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not mention %q", err, fragment)
		}
	}
}

func TestResolveConfigUsesProfile(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, err := resolveConfig([]string{"--config", path, "--dir", ".", "--profile", "local"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "http://127.0.0.1:8080" || cfg.token != "local-token" || cfg.profile != "local" || cfg.appDomain != "localhost" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestResolveConfigUsesDefaultProfile(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, err := resolveConfig([]string{"--config", path, "--dir", "."})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.host != "https://wt.example.com" || cfg.token != "remote-token" || cfg.profile != "remote" || cfg.appDomain != "example.com" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestResolveConfigDropsDomainWhenHostChangesLocality(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, err := resolveConfig([]string{"--config", path, "--dir", ".", "--profile", "remote", "--host", "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.appDomain != "" {
		t.Errorf("app domain = %q, want it cleared for a local supervisor", cfg.appDomain)
	}
}

func TestResolveConfigKeepsExplicitAppDomain(t *testing.T) {
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_TOKEN", "")
	t.Setenv("WT_PROFILE", "")

	path := writeProfileTestConfig(t)

	cfg, err := resolveConfig([]string{"--config", path, "--dir", ".", "--profile", "remote", "--host", "http://127.0.0.1:8080", "--app-domain", "example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.appDomain != "example.com" {
		t.Errorf("app domain = %q, want the explicit value", cfg.appDomain)
	}
}
