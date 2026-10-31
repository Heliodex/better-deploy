// Per-process rlimits applied after the app process starts.

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const defaultNofileLimit = 256

type rlimitSpec struct {
	Resource int
	Name     string
	Value    uint64
}

// rlimitPlan derives the app's per-process hard limits. disk_mb becomes a per-file backstop through RLIMIT_FSIZE.
func rlimitPlan(cfg appConfig) []rlimitSpec {
	plan := []rlimitSpec{
		{Resource: unix.RLIMIT_NOFILE, Name: "RLIMIT_NOFILE", Value: defaultNofileLimit},
	}
	if cfg.DiskMB > 0 {
		plan = append(plan, rlimitSpec{
			Resource: unix.RLIMIT_FSIZE,
			Name:     "RLIMIT_FSIZE",
			Value:    uint64(cfg.DiskMB) << 20,
		})
	}
	return append(plan, rlimitSpec{Resource: unix.RLIMIT_CORE, Name: "RLIMIT_CORE", Value: 0})
}

// applyProcessLimits sets the hard and soft limits on a running child. The caller decides whether a failure is fatal.
func applyProcessLimits(pid int, cfg appConfig) error {
	for _, spec := range rlimitPlan(cfg) {
		limit := &unix.Rlimit{Cur: spec.Value, Max: spec.Value}
		if err := unix.Prlimit(pid, spec.Resource, limit, nil); err != nil {
			return fmt.Errorf("set %s=%d on pid %d: %w", spec.Name, spec.Value, pid, err)
		}
	}
	return nil
}
