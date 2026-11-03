package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertOverview checks that the overview contains every fragment.
func assertOverview(t *testing.T, out string, fragments ...string) {
	t.Helper()

	for _, fragment := range fragments {
		if !strings.Contains(out, fragment) {
			t.Errorf("overview missing %q:\n%s", fragment, out)
		}
	}
}

func TestPrintOverviewWithoutAppOrHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	var out strings.Builder
	printOverview(&out, dir)

	assertOverview(t, out.String(),
		"wt: no command given",
		"directory: "+dir,
		"app: none in this directory",
		"host: not configured",
		"wt help",
		"wt new --dir PATH",
	)
}

func TestPrintOverviewWithLocalAppAndFlagHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"notes","host":"notes.example.com"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_DEPLOY_HOST", "https://wt.example.com")
	t.Setenv("WT_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	var out strings.Builder
	printOverview(&out, dir)

	assertOverview(t, out.String(),
		"app: notes (host notes.example.com)",
		"host: https://wt.example.com",
		"`wt deploy` deploys this app",
	)
}

func TestPrintOverviewUsesConfigHostAndDevHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"notes"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"host":"http://127.0.0.1:8080"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_DEPLOY_HOST", "")
	t.Setenv("WT_CONFIG", configPath)

	var out strings.Builder
	printOverview(&out, dir)

	assertOverview(t, out.String(),
		"app: notes",
		"host: http://127.0.0.1:8080",
		"config: "+configPath,
		"`wt deploy` deploys this app",
	)
}

func TestPrintOverviewHandlesBrokenAppJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_DEPLOY_HOST", "http://127.0.0.1:8080")
	t.Setenv("WT_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	var out strings.Builder
	printOverview(&out, dir)

	assertOverview(t, out.String(),
		"app: app.json:",
		"host: http://127.0.0.1:8080",
		"wt new --dir PATH",
	)
}
