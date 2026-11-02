package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeCgroupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestCgroupManagerWritesLimitsAndCleansUp(t *testing.T) {
	root := fakeCgroupRoot(t)
	manager := newCgroupManager(root)
	cfg := appConfig{CPUMillicores: 500, MemoryMB: 256, PidsMax: 128, IOWeight: 100}
	path, err := manager.create("demo", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "demo") {
		t.Errorf("path = %q", path)
	}
	cases := map[string]string{
		"cpu.max":         "50000 100000",
		"memory.max":      "268435456",
		"memory.swap.max": "0",
		"pids.max":        "128",
		"io.weight":       "default 100",
	}
	for name, want := range cases {
		if got := readTestFile(t, filepath.Join(path, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	subtree := readTestFile(t, filepath.Join(root, "cgroup.subtree_control"))
	for controller := range strings.FieldsSeq(requiredControllers) {
		if !strings.Contains(subtree, "+"+controller) {
			t.Errorf("subtree_control = %q, missing +%s", subtree, controller)
		}
	}

	if err := manager.addPID("demo", 4242); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(path, "cgroup.procs")); got != "4242" {
		t.Errorf("cgroup.procs = %q, want 4242", got)
	}
	// A symlink models cgroup.kill: remove writes 1 into it, then deletes the cgroup directory.
	killTarget := filepath.Join(root, "kill-result")
	if err := os.WriteFile(killTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(killTarget, filepath.Join(path, "cgroup.kill")); err != nil {
		t.Fatal(err)
	}

	if err := manager.remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cgroup directory still exists: %v", err)
	}
	if got := readTestFile(t, killTarget); got != "1" {
		t.Errorf("cgroup.kill received %q, want 1", got)
	}
	if err := manager.remove("demo"); err != nil {
		t.Errorf("removing a missing cgroup should be harmless: %v", err)
	}
}

func TestCgroupManagerReusesExistingDirectory(t *testing.T) {
	root := fakeCgroupRoot(t)
	manager := newCgroupManager(root)
	if _, err := manager.create("demo", appConfig{CPUMillicores: 1000, PidsMax: 32, IOWeight: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.create("demo", appConfig{CPUMillicores: 2000, PidsMax: 64, IOWeight: 75}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "demo")
	if got := readTestFile(t, filepath.Join(path, "cpu.max")); got != "200000 100000" {
		t.Errorf("cpu.max = %q", got)
	}
	if got := readTestFile(t, filepath.Join(path, "pids.max")); got != "64" {
		t.Errorf("pids.max = %q", got)
	}
}

func TestCgroupManagerEnablesParentControllers(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "cgroup.controllers"), []byte("cpu io memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "wt")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real hierarchy repopulates child/cgroup.controllers after the parent enables a controller.
	// Pointing the fake child file at the parent's subtree file models that transition.
	if err := os.Symlink(filepath.Join(parent, "cgroup.subtree_control"), filepath.Join(child, "cgroup.controllers")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := newCgroupManager(child)
	path, err := manager.create("demo", appConfig{CPUMillicores: 1000, PidsMax: 128, IOWeight: 100})
	if err != nil {
		t.Fatalf("create should succeed after enabling the parent: %v", err)
	}
	parentSubtree := readTestFile(t, filepath.Join(parent, "cgroup.subtree_control"))
	for controller := range strings.FieldsSeq(requiredControllers) {
		if !strings.Contains(parentSubtree, "+"+controller) {
			t.Errorf("parent subtree_control = %q, missing +%s", parentSubtree, controller)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("app cgroup was not created: %v", err)
	}
}

func TestCgroupManagerFillsMissingLimitDefaults(t *testing.T) {
	root := fakeCgroupRoot(t)
	manager := newCgroupManager(root)
	path, err := manager.create("demo", appConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(path, "cpu.max")); got != "100000 100000" {
		t.Errorf("cpu.max = %q, want the 1000 millicore default", got)
	}
	if got := readTestFile(t, filepath.Join(path, "pids.max")); got != "128" {
		t.Errorf("pids.max = %q, want 128", got)
	}
	if got := readTestFile(t, filepath.Join(path, "io.weight")); got != "default 100" {
		t.Errorf("io.weight = %q, want default 100", got)
	}
}

func TestCgroupManagerFailsWithoutControllers(t *testing.T) {
	root := t.TempDir()
	manager := newCgroupManager(root)
	if _, err := manager.create("demo", appConfig{CPUMillicores: 1000, PidsMax: 128, IOWeight: 100}); err == nil {
		t.Fatal("expected an error without cgroup.controllers")
	}

	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.create("demo", appConfig{CPUMillicores: 1000, PidsMax: 128, IOWeight: 100}); err == nil {
		t.Fatal("expected an error when pids and io are missing")
	}
}

func TestCPUAndMemoryMaxRendering(t *testing.T) {
	cases := map[int]string{
		1000: "100000 100000",
		500:  "50000 100000",
		1:    "1000 100000",
	}
	for millicores, want := range cases {
		if got := cpuMax(millicores); got != want {
			t.Errorf("cpuMax(%d) = %q, want %q", millicores, got, want)
		}
	}
	if got := memoryMax(0); got != "max" {
		t.Errorf("memoryMax(0) = %q, want max", got)
	}
	if got := memoryMax(64); got != "67108864" {
		t.Errorf("memoryMax(64) = %q", got)
	}
}
