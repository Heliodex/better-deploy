package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeModeFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestEnsureAppReadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeModeFile(t, filepath.Join(root, "main.cwasm"), 0o600)
	writeModeFile(t, filepath.Join(root, "app.json"), 0o600)
	writeModeFile(t, filepath.Join(root, "tool"), 0o700)
	static := filepath.Join(root, "static")
	if err := os.Mkdir(static, 0o700); err != nil {
		t.Fatal(err)
	}
	writeModeFile(t, filepath.Join(static, "app.js"), 0o600)

	if err := ensureAppReadable(root); err != nil {
		t.Fatal(err)
	}
	requireMode(t, root, 0o755)
	requireMode(t, static, 0o755)
	requireMode(t, filepath.Join(root, "main.cwasm"), 0o644)
	requireMode(t, filepath.Join(root, "app.json"), 0o644)
	requireMode(t, filepath.Join(root, "tool"), 0o755)
	requireMode(t, filepath.Join(static, "app.js"), 0o644)
}

func TestEnsureAppReadableLeavesDeployedDirectory0755(t *testing.T) {
	parent := t.TempDir()
	staged, err := os.MkdirTemp(parent, ".demo.upload-")
	if err != nil {
		t.Fatal(err)
	}
	writeModeFile(t, filepath.Join(staged, "main.cwasm"), 0o600)
	if err := ensureAppReadable(staged); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(parent, "demo")
	if err := replaceDir(staged, final); err != nil {
		t.Fatal(err)
	}
	requireMode(t, final, 0o755)
	requireMode(t, filepath.Join(final, "main.cwasm"), 0o644)
}

func TestInstallArtifactMakesDestinationReadable(t *testing.T) {
	src := filepath.Join(t.TempDir(), "cached.cwasm")
	writeModeFile(t, src, 0o600)
	dst := filepath.Join(t.TempDir(), "main.cwasm")
	if err := installArtifact(src, dst); err != nil {
		t.Fatal(err)
	}
	requireMode(t, dst, 0o644)
}

func TestImportCachedArtifactMakesCacheReadable(t *testing.T) {
	s := &supervisor{cfg: config{stateDir: t.TempDir()}}
	src := filepath.Join(t.TempDir(), "uploaded.cwasm")
	writeModeFile(t, src, 0o600)
	key := artifactKey{WasmSHA256: "digest", Wasmtime: "wasmtime 50", Target: "x86_64", Epoch: true}
	dst, err := s.importCachedArtifact(src, key)
	if err != nil {
		t.Fatal(err)
	}
	requireMode(t, dst, 0o644)
}

func TestTrustedPrepareLaunchCreatesNoCgroup(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	root := fakeCgroupRoot(t)
	s.cgroups = newCgroupManager(root)
	a := &app{name: "demo", dir: t.TempDir()}

	iso, err := s.prepareLaunch(a, appConfig{DiskMB: 256})
	if err != nil {
		t.Fatal(err)
	}
	if iso.cgroup != "" {
		t.Errorf("trusted launch created cgroup %q", iso.cgroup)
	}
	if _, err := os.Stat(filepath.Join(root, "demo")); !os.IsNotExist(err) {
		t.Errorf("trusted launch touched the cgroup root: %v", err)
	}
}
