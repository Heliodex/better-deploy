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
	writeModeFile(t, filepath.Join(root, "app.json"), 0o644)
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
	requireMode(t, filepath.Join(root, "app.json"), 0o600)
	requireMode(t, filepath.Join(root, "tool"), 0o755)
	requireMode(t, filepath.Join(static, "app.js"), 0o644)
}

func TestEnsureDirTraversableAddsExecuteWithoutRead(t *testing.T) {
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}

		if err := ensureDirTraversable(dir); err != nil {
			t.Fatal(err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}

		mode := info.Mode().Perm()
		if mode&0o111 != 0o111 {
			t.Errorf("%s mode = %o, want all execute bits", dir, mode)
		}

		if mode&0o044 != 0 {
			t.Errorf("%s mode = %o, must not grant group/other read", dir, mode)
		}

		if mode&0o022 != 0 {
			t.Errorf("%s mode = %o, must not grant group/other write", dir, mode)
		}
	}
}

func TestPersistAppConfigIsPrivateInUntrustedMode(t *testing.T) {
	dir := t.TempDir()

	cfg := appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{"SECRET": "x"}}
	if err := persistAppConfig(dir, cfg, tenantUntrusted); err != nil {
		t.Fatal(err)
	}

	requireMode(t, filepath.Join(dir, "app.json"), 0o600)
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

func TestUntrustedInstallStaticKeepsAppJSONPrivate(t *testing.T) {
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	appDir := t.TempDir()
	a := &app{name: "demo", dir: appDir, cfg: appConfig{Name: "demo", Host: "demo.localhost"}}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "app.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{"SECRET": "x"}}
	if err := s.installStatic(a, cfg, source); err != nil {
		t.Fatal(err)
	}

	requireMode(t, filepath.Join(appDir, "app.json"), 0o600)
}

func TestImportCachedArtifactMakesCacheReadable(t *testing.T) {
	s := &supervisor{cfg: config{stateDir: t.TempDir()}}
	src := filepath.Join(t.TempDir(), "uploaded.cwasm")
	writeModeFile(t, src, 0o600)

	key := artifactKey{WasmSHA256: "digest", Wasmtime: "wasmtime 50", Target: "x86_64", CPU: artifactCPUFeatures, Epoch: true}

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

	iso, err := s.prepareIsolation(a, appConfig{DiskMB: 256})
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
