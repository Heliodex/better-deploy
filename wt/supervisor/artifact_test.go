package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// digestText returns the content digest the supervisor uses for an in-memory wasm.
func digestText(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// writeCompiler installs a fake wasmtime that reports a version and writes a new artifact on compile.
func writeCompiler(t *testing.T, version, output string, fail bool) string {
	t.Helper()
	return writeCompilerWithCounter(t, version, output, "", fail)
}

// writeCompilerWithCounter also counts compile calls, so a test can prove a cache hit skipped the compiler.
func writeCompilerWithCounter(t *testing.T, version, output, counter string, fail bool) string {
	t.Helper()
	record := ""
	if counter != "" {
		record = "  printf x >> '" + counter + "'\n"
	}
	script := `#!/bin/sh
case "$1" in
--version)
  echo "` + version + `"
  exit 0
  ;;
compile)
` + record + `  if [ "` + boolText(fail) + `" = "yes" ]; then
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

func artifactSupervisor(t *testing.T, compiler string) *supervisor {
	t.Helper()
	return artifactSupervisorWithCache(t, compiler, t.TempDir())
}

func artifactSupervisorWithCache(t *testing.T, compiler, cache string) *supervisor {
	t.Helper()
	return &supervisor{cfg: config{
		stateDir:         t.TempDir(),
		cacheDir:         cache,
		cacheMaxBytes:    defaultCacheMaxBytes,
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

func compileCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

func TestEnsureArtifactRebuildsStaleArtifact(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	dir := writeBundleDir(t, "wasm", "old", `{"wasmtime":"wasmtime-cli 50.0.0","target":"x86_64-test"}`)

	epoch, err := artifactSupervisor(t, compiler).ensureArtifact(dir, false)
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
	if artifact.WasmSHA256 != digestText("wasm") || artifact.WasmBytes != int64(len("wasm")) {
		t.Errorf("artifact wasm metadata = %+v, want digest %s and %d bytes", artifact, digestText("wasm"), len("wasm"))
	}
}

func TestEnsureArtifactKeepsMatchingArtifact(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "compiles")
	compiler := writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "rebuilt", counter, false)
	wasm := "wasm"
	artifact := fmt.Sprintf(`{"wasmtime":"wasmtime-cli 51.0.0","target":"x86_64-test","epoch_interruption":true,"wasm_sha256":"%s","wasm_bytes":%d}`, digestText(wasm), len(wasm))
	dir := writeBundleDir(t, wasm, "current", artifact)

	epoch, err := artifactSupervisor(t, compiler).ensureArtifact(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !epoch {
		t.Error("matching epoch artifact was not reported")
	}
	if got := compileCount(t, counter); got != 0 {
		t.Errorf("compile calls = %d, want 0 for a matching artifact", got)
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

	epoch, err := artifactSupervisor(t, compiler).ensureArtifact(dir, true)
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

func TestEnsureArtifactRejectsCwasmForDifferentWasm(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "", true)
	artifact := fmt.Sprintf(`{"wasmtime":"wasmtime-cli 51.0.0","target":"x86_64-test","epoch_interruption":true,"wasm_sha256":"%s"}`, digestText("other"))
	dir := writeBundleDir(t, "wasm", "old", artifact)

	_, err := artifactSupervisor(t, compiler).ensureArtifact(dir, false)
	if err == nil || !strings.Contains(err.Error(), "not "+digestText("wasm")) {
		t.Errorf("ensureArtifact error = %v, want a stale-wasm error", err)
	}
}

func TestEnsureArtifactFailsWithoutCompilerOrCwasm(t *testing.T) {
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "", true)
	dir := writeBundleDir(t, "wasm", "", "")

	_, err := artifactSupervisor(t, compiler).ensureArtifact(dir, false)
	if err == nil || !strings.Contains(err.Error(), "compile main.wasm") {
		t.Errorf("ensureArtifact error = %v, want a compile failure", err)
	}
}

func TestEnsureArtifactLegacyBundle(t *testing.T) {
	// Bundles uploaded before main.wasm was kept still start from their cwasm.
	compiler := writeCompiler(t, "wasmtime-cli 51.0.0", "rebuilt", false)
	dir := writeBundleDir(t, "", "legacy", "")
	if epoch, err := artifactSupervisor(t, compiler).ensureArtifact(dir, true); err != nil || epoch {
		t.Errorf("legacy bundle = %v, %v; want no epoch and no error", epoch, err)
	}

	// A directory with neither artifact is not a component.
	if _, err := artifactSupervisor(t, compiler).ensureArtifact(t.TempDir(), false); !errors.Is(err, errNoComponent) {
		t.Errorf("empty directory error = %v, want errNoComponent", err)
	}
}

func TestArtifactMatches(t *testing.T) {
	digest := digestText("wasm")
	current := artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test", Epoch: true, WasmSHA256: digest}
	cases := []struct {
		name     string
		recorded artifactInfo
		current  artifactInfo
		want     bool
	}{
		{"same", current, current, true},
		{"older runtime", artifactInfo{Wasmtime: "wasmtime-cli 50.0.0", Target: "x86_64-test", Epoch: true, WasmSHA256: digest}, current, false},
		{"other target", artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "aarch64-test", Epoch: true, WasmSHA256: digest}, current, false},
		{"no digest", artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test", Epoch: true}, current, false},
		{"other wasm", artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test", Epoch: true, WasmSHA256: digestText("other")}, current, false},
		{"no epoch", artifactInfo{Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test", WasmSHA256: digest}, current, false},
		{"unknown recorded version", artifactInfo{Target: "x86_64-test", Epoch: true, WasmSHA256: digest}, current, true},
		{"unknown current version", artifactInfo{Wasmtime: "wasmtime-cli 50.0.0", Target: "x86_64-test", Epoch: true, WasmSHA256: digest}, artifactInfo{Target: "x86_64-test", Epoch: true, WasmSHA256: digest}, true},
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
	// Matching runtime, target and wasm, but without the instrumentation a per-request timeout needs.
	artifact := fmt.Sprintf(`{"wasmtime":"wasmtime-cli 51.0.0","target":"x86_64-test","wasm_sha256":"%s"}`, digestText("wasm"))
	dir := writeBundleDir(t, "wasm", "old", artifact)

	epoch, err := artifactSupervisor(t, compiler).ensureArtifact(dir, true)
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
	artifactInfo, err := readArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !artifactInfo.Epoch {
		t.Errorf("artifact = %+v, want epoch support recorded", artifactInfo)
	}
}

func TestEnsureArtifactReusesCache(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "compiles")
	compiler := writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "compiled", counter, false)
	cache := t.TempDir()

	first := writeBundleDir(t, "wasm", "", "")
	if _, err := artifactSupervisorWithCache(t, compiler, cache).ensureArtifact(first, false); err != nil {
		t.Fatal(err)
	}
	if got := compileCount(t, counter); got != 1 {
		t.Fatalf("compile calls = %d, want 1 after the first build", got)
	}

	second := writeBundleDir(t, "wasm", "", "")
	if _, err := artifactSupervisorWithCache(t, compiler, cache).ensureArtifact(second, false); err != nil {
		t.Fatal(err)
	}
	if got := compileCount(t, counter); got != 1 {
		t.Errorf("compile calls = %d, want the cache hit to skip compiling", got)
	}
	cwasm, err := os.ReadFile(filepath.Join(second, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cwasm) != "compiled" {
		t.Errorf("main.cwasm = %q, want the cached artifact", cwasm)
	}
	artifact, err := readArtifact(second)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.WasmSHA256 != digestText("wasm") || artifact.WasmBytes != int64(len("wasm")) {
		t.Errorf("artifact = %+v, want wasm digest and size", artifact)
	}
}

func TestEnsureArtifactCacheInvalidatesOnRuntimeAndTarget(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "compiles")
	cache := t.TempDir()
	oldCompiler := writeCompilerWithCounter(t, "wasmtime-cli 50.0.0", "old", counter, false)
	newCompiler := writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "new", counter, false)

	if _, err := artifactSupervisorWithCache(t, oldCompiler, cache).ensureArtifact(writeBundleDir(t, "wasm", "", ""), false); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactSupervisorWithCache(t, newCompiler, cache).ensureArtifact(writeBundleDir(t, "wasm", "", ""), false); err != nil {
		t.Fatal(err)
	}
	if got := compileCount(t, counter); got != 2 {
		t.Fatalf("compile calls = %d, want a runtime version change to invalidate the cache", got)
	}

	otherTarget := artifactSupervisorWithCache(t, newCompiler, cache)
	otherTarget.cfg.wasmtimeTarget = "aarch64-test"
	dir := writeBundleDir(t, "wasm", "", "")
	if _, err := otherTarget.ensureArtifact(dir, false); err != nil {
		t.Fatal(err)
	}
	if got := compileCount(t, counter); got != 3 {
		t.Errorf("compile calls = %d, want a target change to invalidate the cache", got)
	}
}

func TestArtifactKeyIncludesEpoch(t *testing.T) {
	key := artifactKey{WasmSHA256: digestText("wasm"), Wasmtime: "wasmtime-cli 51.0.0", Target: "x86_64-test", Epoch: true}
	other := key
	other.Epoch = false
	if key.digest() == other.digest() {
		t.Error("artifact key does not change with the epoch setting")
	}
}

func TestPruneArtifactCacheRemovesOldestAndKeepsNewEntry(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "cwasm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, name := range []string{"oldest.cwasm", "middle.cwasm", "newest.cwasm"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(time.Duration(i-2) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	s := artifactSupervisorWithCache(t, "wasmtime", cache)
	s.cfg.cacheMaxBytes = 25
	keep := filepath.Join(dir, "newest.cwasm")
	s.pruneArtifactCache(keep)

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("new cache entry was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oldest.cwasm")); !os.IsNotExist(err) {
		t.Errorf("oldest cache entry still exists: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("cache entries = %d, want the two newest after pruning", len(entries))
	}
}
