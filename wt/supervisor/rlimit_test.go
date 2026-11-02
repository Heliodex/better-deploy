package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestRlimitPlan(t *testing.T) {
	plan := rlimitPlan(appConfig{DiskMB: 256})

	want := map[int]uint64{
		unix.RLIMIT_NOFILE: 1024,
		unix.RLIMIT_FSIZE:  256 << 20,
		unix.RLIMIT_CORE:   0,
	}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d entries, want %d: %+v", len(plan), len(want), plan)
	}

	for _, spec := range plan {
		value, ok := want[spec.Resource]
		if !ok {
			t.Errorf("unexpected resource %s", spec.Name)
			continue
		}

		if spec.Value != value {
			t.Errorf("%s = %d, want %d", spec.Name, spec.Value, value)
		}
	}
}

func TestRlimitPlanWithoutDiskLimit(t *testing.T) {
	plan := rlimitPlan(appConfig{})
	for _, spec := range plan {
		if spec.Resource == unix.RLIMIT_FSIZE {
			t.Errorf("unexpected RLIMIT_FSIZE without disk_mb: %+v", spec)
		}
	}
}
