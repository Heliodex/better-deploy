package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAppArgs(t *testing.T) {
	state := t.TempDir()
	a := &app{
		name: "demo",
		cfg: appConfig{
			Runtime: runtimeNative,
			MemoryMB: 64,
			CPUMillicores: 1500,
		},
	}
	s := &supervisor{
		cfg: config{
			stateDir: state,
		},
	}

	args, env, err := s.appArgsFor(a, "main", false)
	if err != nil {
		t.Fatal(err)
	}

	if len(args) != 0 {
		t.Errorf("native args = %v, want none", args)
	}

	joined := strings.Join(env, "\n")

	for _, want := range []string{"WT_APP_LISTEN=127.0.0.1:0", "GOMEMLIMIT=64MiB", "GOMAXPROCS=2", "WT_DATA_DIR="} {
		if !strings.Contains(joined, want) {
			t.Errorf("env is missing %s:\n%s", want, joined)
		}
	}

	if strings.Contains(joined, "max-memory-size") {
		t.Fatalf("native env contains a wasmtime flag:\n%s", joined)
	}
}

func TestNormaliseRuntime(t *testing.T) {
	cfg := appConfig{
		Name: "demo",
		Host: "demo.localhost",
	}

	normalised, err := normaliseAppConfig(cfg, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}

	if normalised.Runtime != runtimeWasm {
		t.Errorf("default runtime = %q", normalised.Runtime)
	}

	cfg.Runtime = runtimeNative

	normalised, err = normaliseAppConfig(cfg, "demo", "localhost", tenantTrusted)
	if err != nil {
		t.Fatal(err)
	}

	if normalised.Runtime != runtimeNative {
		t.Errorf("native runtime = %q", normalised.Runtime)
	}

	if _, err := normaliseAppConfig(cfg, "demo", "localhost", tenantUntrusted); err == nil {
		t.Fatal("native runtime should be rejected in untrusted mode")
	}
}

// staticTestBinary builds a minimal CGO-free Go executable for the native artifact tests.
func staticTestBinary(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module native.test\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "main")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build static test binary: %v\n%s", err, out)
	}

	return bin
}

func TestEnsureNativeArtifact(t *testing.T) {
	dir := t.TempDir()

	self, err := os.ReadFile(staticTestBinary(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main"), self, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureNativeArtifact(dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "main"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("main mode = %o, want executable", info.Mode().Perm())
	}

	bad := t.TempDir()

	if err := os.WriteFile(filepath.Join(bad, "main"), []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureNativeArtifact(bad); err == nil {
		t.Fatal("a non-ELF main should be rejected")
	}
}

func TestNativeNamespacePlan(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "apps", "demo")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	self, err := os.ReadFile(staticTestBinary(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main"), self, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(state, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &app{
		name: "demo",
		dir: dir,
		cfg: appConfig{
			Runtime: runtimeNative,
		},
	}
	s := &supervisor{
		cfg: config{
			stateDir: state,
			wasmtime: "wasmtime",
		},
	}

	plan, err := s.buildNamespacePlan(a, filepath.Join(state, ".ns", "demo"), false)
	if err != nil {
		t.Fatal(err)
	}

	if !plan.Native || plan.BinPath != "/app/main" || plan.Wasmtime != "" || !plan.Seccomp {
		t.Errorf("native plan = %+v", plan)
	}
}
