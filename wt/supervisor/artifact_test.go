package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCompiler installs a fake wasmtime that reports a version and writes a new artifact on compile.
func writeCompiler(t *testing.T, version, output string, fail bool) string {
	t.Helper()
	script := `#!/bin/sh
case "$1" in
--version)
  echo "` + version + `"
  exit 0
  ;;
compile)
  if [ "` + boolText(fail) + `" = "yes" ]; then
    echo "compile is not enabled in this build" >&2
    exit 1
  fi
  out=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "-o" ]; then
      out="$2"
      shift
    fi
    shift
  done
  printf '%s' '` + output + `' > "$out"
  ;;
esac
`
	path := filepath.Join(t.TempDir(), "wasmtime")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func boolText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func artifactSupervisor(compiler string) *supervisor {
	return &supervisor{cfg: config{
		wasmtime:         compiler,
		wasmtimeCompiler: compiler,
		wasmtimeTarget:   "x86_64-test",
	}}
}

func writeBundleDir(t *testing.T, wasm, cwasm, artifact string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"main.wasm": wasm, "main.cwasm": cwasm, artifactName: artifact} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestEnsureArtifactRebuildsStaleArtifact(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	dir := writeBundleDir(t, "wasm", "old", `{"wasmtime":"wasmtime-cli 50.0.0","target":"x86_64-test"}`)

	epoch, err := artifactSupervisor(compiler).ensureArtifact(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !epoch {
		t.Error("rebuild did not report epoch support")
	}
	cwasm, err := os.ReadFile(filepath.Join(dir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cwasm) != "rebuilt" {
		t.Errorf("main.cwasm = %q, want rebuilt", cwasm)
	}
	artifact, err := readArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Wasmtime != "wasmtime-cli 51.0.0" || artifact.Target != "x86_64-test" || artifact.CompiledAt.IsZero() || !artifact.Epoch {
		t.Errorf("artifact = %+v", artifact)
	}
}

func TestEnsureArtifactKeepsMatchingArtifact(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	dir := writeBundleDir(t, "wasm", "current", `{"wasmtime":"wasmtime-cli 51.0.0","target":"x86_64-test","epoch_interruption":true}`)

	epoch, err := artifactSupervisor(compiler).ensureArtifact(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !epoch {
		t.Error("matching epoch artifact was not reported")
	}
	cwasm, err := os.ReadFile(filepath.Join(dir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cwasm) != "current" {
		t.Errorf("main.cwasm = %q, want the existing artifact", cwasm)
	}
}

func TestEnsureArtifactKeepsCwasmWithoutCompiler(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "", true)
	dir := writeBundleDir(t, "wasm", "old", `{"wasmtime":"wasmtime-cli 50.0.0","target":"x86_64-test"}`)

	epoch, err := artifactSupervisor(compiler).ensureArtifact(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if epoch {
		t.Error("failed rebuild claimed epoch support")
	}
	cwasm, err := os.ReadFile(filepath.Join(dir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cwasm) != "old" {
		t.Errorf("main.cwasm = %q, want the existing artifact", cwasm)
	}
}

func TestEnsureArtifactLegacyBundle(t *testing.T) {
	// Bundles uploaded before main.wasm was kept still start from their cwasm.
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	dir := writeBundleDir(t, "", "legacy", "")
	if epoch, err := artifactSupervisor(compiler).ensureArtifact(dir, true); err != nil || epoch {
		t.Errorf("legacy bundle = %v, %v; want no epoch and no error", epoch, err)
	}

	// A directory with neither artifact is not a component.
	if _, err := artifactSupervisor(compiler).ensureArtifact(t.TempDir(), false); !errors.Is(err, errNoComponent) {
		t.Errorf("empty directory error = %v, want errNoComponent", err)
	}
}

func TestArtifactMatches(t *testing.T) {
	current := artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test"}
	cases := []struct {
		name     string
		recorded artifactInfo
		current  artifactInfo
		want     bool
	}{
		{"same", current, current, true},
		{"older runtime", artifactInfo{Wasmtime: "wasmtime-cli 50.0.0", Target: "x86_64-test"}, current, false},
		{"other target", artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "aarch64-test"}, current, false},
		{"unknown recorded version", artifactInfo{Target: "x86_64-test"}, current, true},
		{"unknown current version", artifactInfo{Wasmtime: "wasmtime-cli 50.0.0", Target: "x86_64-test"}, artifactInfo{Target: "x86_64-test"}, true},
	}
	for _, tc := range cases {
		if got := artifactMatches(tc.recorded, tc.current); got != tc.want {
			t.Errorf("%s: artifactMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadArtifactRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, artifactName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifact(dir); err == nil || !strings.Contains(err.Error(), artifactName) {
		t.Errorf("readArtifact error = %v", err)
	}
}

func TestEnsureArtifactRebuildsForEpoch(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	// Matching runtime and target, but without the instrumentation a per-request timeout needs.
	dir := writeBundleDir(t, "wasm", "old", `{"wasmtime":"wasmtime-cli 51.0.0","target":"x86_64-test"}`)

	epoch, err := artifactSupervisor(compiler).ensureArtifact(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !epoch {
		t.Error("epoch rebuild was not reported")
	}
	cwasm, err := os.ReadFile(filepath.Join(dir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cwasm) != "rebuilt" {
		t.Errorf("main.cwasm = %q, want rebuilt", cwasm)
	}
	artifact, err := readArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.Epoch {
		t.Errorf("artifact = %+v, want epoch support recorded", artifact)
	}
}
