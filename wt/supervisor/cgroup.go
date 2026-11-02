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

// missingControllersError reports which controllers a cgroup directory does not expose.
type missingControllersError struct {
	dir     string
	missing []string
}

func (e *missingControllersError) Error() string {
	return fmt.Sprintf("cgroup controllers %s are not available under %s", strings.Join(e.missing, ", "), e.dir)
}

// enableControllersAt enables required controllers for that directory's children.
func enableControllersAt(dir string, required []string) error {
	controllers, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup controllers under %s (cgroup v2 required): %w", dir, err)
	}

	available := map[string]bool{}
	for name := range strings.FieldsSeq(string(controllers)) {
		available[strings.TrimPrefix(name, "+")] = true
	}

	var missing []string

	for _, name := range required {
		if !available[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return &missingControllersError{dir: dir, missing: missing}
	}

	subtreePath := filepath.Join(dir, "cgroup.subtree_control")

	existing, err := os.ReadFile(subtreePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", subtreePath, err)
	}
	// cgroup.subtree_control reads back bare names ("cpu memory") but only accepts "+name"/"-name" writes.
	enabled := map[string]bool{}
	tokens := make([]string, 0, len(required))

	for token := range strings.FieldsSeq(string(existing)) {
		name := strings.TrimPrefix(token, "+")
		enabled[name] = true
		tokens = append(tokens, "+"+name)
	}

	changed := false

	for _, name := range required {
		if !enabled[name] {
			tokens = append(tokens, "+"+name)
			enabled[name] = true
			changed = true
		}
	}

	if changed || len(strings.Fields(string(existing))) == 0 {
		if err := os.WriteFile(subtreePath, []byte(strings.Join(tokens, " ")+"\n"), 0o600); err != nil {
			return fmt.Errorf("enable cgroup controllers under %s: %w", dir, err)
		}
	}

	return nil
}

// enableControllers turns on cpu, memory, pids and io for child cgroups under the manager root.
// A freshly created WT_CGROUP_ROOT often has an empty cgroup.controllers until its parent enables them,
// so try the parent once and re-read before failing closed.
func (m *cgroupManager) enableControllers() error {
	if strings.TrimSpace(m.root) == "" {
		return errors.New("WT_CGROUP_ROOT is empty")
	}

	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return fmt.Errorf("create cgroup root %s: %w", m.root, err)
	}

	required := strings.Fields(requiredControllers)

	err := enableControllersAt(m.root, required)
	if err == nil {
		return nil
	}

	if _, ok := errors.AsType[*missingControllersError](err); !ok {
		return err
	}

	parent := filepath.Dir(m.root)
	if parent == m.root {
		return err
	}

	if _, statErr := os.Stat(filepath.Join(parent, "cgroup.controllers")); statErr != nil {
		return err
	}

	if parentErr := enableControllersAt(parent, required); parentErr != nil {
		return fmt.Errorf("%w (enabling parent %s: %w)", err, parent, parentErr)
	}

	if err := enableControllersAt(m.root, required); err != nil {
		return fmt.Errorf("cgroup controllers under %s after enabling parent %s: %w", m.root, parent, err)
	}

	return nil
}

// cpuMax renders cpu.max for a millicore quota over a 100 ms period (1000 millicores means one core).
func cpuMax(millicores int) string {
	quota := max(int64(millicores)*cgroupCPUPeriod/1000, 1000)
	return fmt.Sprintf("%d %d", quota, cgroupCPUPeriod)
}

// memoryMax renders cgroup v2 memory.max, or "max" when no limit is set.
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
		if err := os.WriteFile(filepath.Join(path, file.name), []byte(file.value+"\n"), 0o600); err != nil {
			_ = os.RemoveAll(path)
			return "", fmt.Errorf("write %s in %s: %w", file.name, path, err)
		}
	}

	return path, nil
}

// addPID places the started process in the app's cgroup.
func (m *cgroupManager) addPID(name string, pid int) error {
	path := filepath.Join(m.path(name), "cgroup.procs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("add pid %d to %s: %w", pid, path, err)
	}

	return nil
}

// remove kills any stray processes left in the app cgroup, then deletes it.
func (m *cgroupManager) remove(name string) error {
	path := m.path(name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("stat cgroup %s: %w", path, err)
	}

	kill := filepath.Join(path, "cgroup.kill")
	if _, err := os.Stat(kill); err == nil {
		if err := os.WriteFile(kill, []byte("1"), 0o600); err != nil {
			return fmt.Errorf("kill cgroup %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", kill, err)
	}

	err := os.Remove(path)
	switch {
	case err == nil:
		return nil
	case os.IsNotExist(err):
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
