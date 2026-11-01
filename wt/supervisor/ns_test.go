//go:build linux

package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestMergeEnvOverrideWins(t *testing.T) {
	merged := mergeEnv(
		[]string{"PATH=/bin", "TMPDIR=/old", "LANG=C"},
		[]string{"TMPDIR=/tmp", "HOME=/data"},
	)
	want := []string{"HOME=/data", "LANG=C", "PATH=/bin", "TMPDIR=/tmp"}
	if !slices.Equal(merged, want) {
		t.Errorf("mergeEnv = %v, want %v", merged, want)
	}
}

func TestAppArgsForNamespaceUsesInsidePaths(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	a := &app{
		name: "demo",
		dir:  t.TempDir(),
		cfg:  appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{}},
	}
	args, env, err := s.appArgsFor(a, filepath.Join(a.dir, "main.cwasm"), true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--dir /data::/data", "--env TMPDIR", "--env HOME", " /app/main.cwasm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, a.dir) || strings.Contains(joined, s.cfg.stateDir) {
		t.Errorf("host paths leaked into namespaced args: %q", joined)
	}
	if !slices.Contains(env, "TMPDIR=/tmp") || !slices.Contains(env, "HOME=/data") {
		t.Errorf("env = %v, want TMPDIR=/tmp and HOME=/data", env)
	}
}

func TestAppArgsForNamespaceLetsAppOverrideDefaults(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	a := &app{
		name: "demo",
		dir:  t.TempDir(),
		cfg:  appConfig{Name: "demo", Host: "demo.localhost", Env: map[string]string{"TMPDIR": "/custom"}},
	}
	_, env, err := s.appArgsFor(a, "host/main.cwasm", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "TMPDIR=/custom") {
		t.Errorf("env = %v, want the app's TMPDIR", env)
	}
}

func TestNamespaceSysProcAttr(t *testing.T) {
	attr := namespaceSysProcAttr(20001, 20002, true)
	wantFlags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
	if attr.Cloneflags != wantFlags {
		t.Errorf("clone flags = %#x, want %#x", attr.Cloneflags, wantFlags)
	}
	if !attr.GidMappingsEnableSetgroups {
		t.Error("untrusted namespaces should allow the helper to clear groups")
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0].HostID != 20001 || attr.UidMappings[0].ContainerID != 0 {
		t.Errorf("uid mappings = %+v", attr.UidMappings)
	}
	if len(attr.GidMappings) != 1 || attr.GidMappings[0].HostID != 20002 || attr.GidMappings[0].ContainerID != 0 {
		t.Errorf("gid mappings = %+v", attr.GidMappings)
	}
}

func TestNamespacePlanUsesDynamicRuntimeLibs(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	s.cfg.wasmtime = os.Args[0]
	app := &app{name: "demo", dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.cfg.stateDir, app.name), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := s.buildNamespacePlan(app, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BinPath != "/bin/wasmtime" || plan.Hostname != "demo" {
		t.Errorf("plan = %+v", plan)
	}
	if !plan.Dynamic {
		t.Error("the test binary should be dynamically linked")
	}
	if len(plan.LibDirs) == 0 {
		t.Error("a dynamic runtime needs library directories")
	}
	if !slices.Contains(plan.Files, "/etc/passwd") {
		t.Errorf("files = %v, want /etc/passwd", plan.Files)
	}
}

func TestNamespacePlanCarriesRlimits(t *testing.T) {
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	s.cfg.stateDir = t.TempDir()
	s.cfg.wasmtime = os.Args[0]
	app := &app{name: "demo", dir: t.TempDir(), cfg: appConfig{DiskMB: 256}}
	if err := os.MkdirAll(filepath.Join(s.cfg.stateDir, app.name), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := s.buildNamespacePlan(app, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rlimits) != 3 {
		t.Fatalf("rlimits = %+v, want three entries", plan.Rlimits)
	}
	if plan.Rlimits[0].Value != defaultNofileLimit || plan.Rlimits[1].Value != 256<<20 || plan.Rlimits[2].Value != 0 {
		t.Errorf("rlimits = %+v", plan.Rlimits)
	}
}

func TestElfInterpRejectsScripts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-wasmtime")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	interp, isELF, err := elfInterp(path)
	if err != nil {
		t.Fatal(err)
	}
	if isELF || interp != "" {
		t.Errorf("script reported as ELF: interp=%q isELF=%t", interp, isELF)
	}
}

func TestResetNamespaceRoots(t *testing.T) {
	dir := t.TempDir()
	if err := resetNamespaceRoots(dir, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ".ns"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Errorf("untrusted .ns mode = %o, want 711", info.Mode().Perm())
	}
	if err := resetNamespaceRoots(dir, false); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(dir, ".ns"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("trusted .ns mode = %o, want 700", info.Mode().Perm())
	}
}

func TestValidateIsolationConfig(t *testing.T) {
	untrusted := config{tenantMode: tenantUntrusted, appNamespaces: true, appSeccomp: true}
	if err := validateIsolationConfig(untrusted); err != nil {
		t.Errorf("both boundaries enabled: %v", err)
	}
	untrusted.appNamespaces = false
	if err := validateIsolationConfig(untrusted); err == nil {
		t.Error("untrusted mode accepted namespaces off")
	}
	untrusted.appNamespaces = true
	untrusted.appSeccomp = false
	if err := validateIsolationConfig(untrusted); err == nil {
		t.Error("untrusted mode accepted seccomp off")
	}
	if err := validateIsolationConfig(config{tenantMode: tenantTrusted}); err != nil {
		t.Errorf("trusted mode should keep both optional: %v", err)
	}
}

func TestNamespaceHelperChild(t *testing.T) {
	if os.Getenv("WT_NS_HELPER") != "1" {
		t.Skip("namespace helper child")
	}
	nsInitMain([]string{"-c", "test -d /app && test -w /data && echo ok > /data/ns-ok && test ! -e /etc/hostname"})
}

// TestNamespaceLaunch runs a real shell inside the namespace.
// It is opt-in because user namespaces are often disabled in CI and the test needs a non-root supervisor to avoid mapping container uid 0 to host root.
func TestNamespaceLaunch(t *testing.T) {
	if os.Getenv("WT_NS_TEST") != "1" {
		t.Skip("set WT_NS_TEST=1 to run the namespace integration test")
	}
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if os.Geteuid() == 0 {
		t.Skip("run as a non-root user so container uid 0 does not map to host root")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is not available")
	}

	root, err := os.MkdirTemp("", "wt-ns-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	appDir := t.TempDir()
	dataDir := t.TempDir()

	interp, isELF, err := elfInterp("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if !isELF {
		t.Skip("/bin/sh is not an ELF binary")
	}
	plan := nsPlan{
		Root:        root,
		Hostname:    "wt-ns-test",
		AppDir:      appDir,
		DataDir:     dataDir,
		Wasmtime:    "/bin/sh",
		BinPath:     "/bin/sh",
		Dynamic:     interp != "",
		Files:       namespaceFiles(interp != ""),
		LibDirs:     namespaceLibDirs(interp),
		TmpfsMB:     defaultTmpfsMB,
		ClearGroups: false,
		Seccomp:     true,
		Rlimits:     rlimitPlan(appConfig{}),
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNamespaceHelperChild$")
	cmd.Env = append(os.Environ(), "WT_NS_HELPER=1", nsPlanEnv+"="+string(raw))
	cmd.SysProcAttr = namespaceSysProcAttr(os.Geteuid(), os.Getegid(), false)

	goReader, goWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pidSink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{pidSink, goReader}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = goReader.Close()
	_ = pidSink.Close()
	_, _ = goWriter.Write([]byte{1})
	_ = goWriter.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("namespace helper: %v\n%s", err, out.String())
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "ns-ok"))
	if err != nil {
		t.Fatalf("app did not write /data: %v\n%s", err, out.String())
	}
	if strings.TrimSpace(string(data)) != "ok" {
		t.Errorf("data = %q, want ok", data)
	}
}
