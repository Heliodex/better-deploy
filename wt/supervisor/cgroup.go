// cgroup v2 limits for per-app processes.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	cgroupCPUPeriod = 100000
	// requiredControllers are the limits the untrusted mode relies on. Missing ones fail the launch.
	requiredControllers = "cpu memory pids io"
)

type cgroupManager struct {
	root string
}

func newCgroupManager(root string) *cgroupManager {
	return &cgroupManager{root: root}
}

func (m *cgroupManager) path(name string) string {
	return filepath.Join(m.root, name)
}

// enableControllers turns on cpu, memory, pids and io for child cgroups under the manager root.
func (m *cgroupManager) enableControllers() error {
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return fmt.Errorf("create cgroup root %s: %w", m.root, err)
	}
	controllers, err := os.ReadFile(filepath.Join(m.root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup controllers under %s (cgroup v2 required): %w", m.root, err)
	}
	available := map[string]bool{}
	for _, name := range strings.Fields(string(controllers)) {
		available[name] = true
	}
	for _, name := range strings.Fields(requiredControllers) {
		if !available[name] {
			return fmt.Errorf("cgroup controller %q is not available under %s", name, m.root)
		}
	}

	subtreePath := filepath.Join(m.root, "cgroup.subtree_control")
	existing, err := os.ReadFile(subtreePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", subtreePath, err)
	}
	// cgroup.subtree_control reads back bare names ("cpu memory") but only accepts "+name"/"-name" writes.
	enabled := map[string]bool{}
	tokens := make([]string, 0, 4)
	for _, token := range strings.Fields(string(existing)) {
		name := strings.TrimPrefix(token, "+")
		enabled[name] = true
		tokens = append(tokens, "+"+name)
	}
	changed := false
	for _, name := range strings.Fields(requiredControllers) {
		if !enabled[name] {
			tokens = append(tokens, "+"+name)
			enabled[name] = true
			changed = true
		}
	}
	if changed || len(strings.Fields(string(existing))) == 0 {
		if err := os.WriteFile(subtreePath, []byte(strings.Join(tokens, " ")+"\n"), 0o644); err != nil {
			return fmt.Errorf("enable cgroup controllers under %s: %w", m.root, err)
		}
	}
	return nil
}

// cpuMax renders cpu.max for a millicore quota over a 100 ms period (1000 millicores means one core).
func cpuMax(millicores int) string {
	quota := int64(millicores) * cgroupCPUPeriod / 1000
	if quota < 1000 {
		quota = 1000
	}
	return fmt.Sprintf("%d %d", quota, cgroupCPUPeriod)
}

func memoryMax(memoryMB int) string {
	if memoryMB <= 0 {
		return "max"
	}
	return strconv.FormatInt(int64(memoryMB)<<20, 10)
}

// create makes the app's cgroup and writes its limits, reusing a directory left by a previous run.
func (m *cgroupManager) create(name string, cfg appConfig) (string, error) {
	if err := m.enableControllers(); err != nil {
		return "", err
	}
	path := m.path(name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup %s: %w", path, err)
	}
	cpu := cfg.CPUMillicores
	if cpu <= 0 {
		cpu = defaultCPUMillicores
	}
	pids := cfg.PidsMax
	if pids <= 0 {
		pids = defaultPidsMax
	}
	ioWeight := cfg.IOWeight
	if ioWeight <= 0 {
		ioWeight = defaultIOWeight
	}
	files := []struct{ name, value string }{
		{"cpu.max", cpuMax(cpu)},
		{"memory.max", memoryMax(cfg.MemoryMB)},
		{"memory.swap.max", "0"},
		{"pids.max", strconv.Itoa(pids)},
		{"io.weight", fmt.Sprintf("default %d", ioWeight)},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(path, file.name), []byte(file.value+"\n"), 0o644); err != nil {
			_ = os.RemoveAll(path)
			return "", fmt.Errorf("write %s in %s: %w", file.name, path, err)
		}
	}
	return path, nil
}

// addPID places the started process in the app's cgroup.
func (m *cgroupManager) addPID(name string, pid int) error {
	path := filepath.Join(m.path(name), "cgroup.procs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("add pid %d to %s: %w", pid, path, err)
	}
	return nil
}

// remove deletes an app's cgroup. The kernel only allows rmdir once it has no processes.
func (m *cgroupManager) remove(name string) error {
	path := m.path(name)
	err := os.Remove(path)
	switch {
	case err == nil, os.IsNotExist(err):
		return nil
	case errors.Is(err, syscall.ENOTEMPTY):
		// A filesystem-backed test root keeps its limit files; a real cgroup directory does not.
		return os.RemoveAll(path)
	default:
		return fmt.Errorf("remove cgroup %s: %w", path, err)
	}
}

// close removes every app cgroup under the root, leaving the root itself in place.
func (m *cgroupManager) close() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			_ = m.remove(entry.Name())
		}
	}
}
