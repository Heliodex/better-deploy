package main

import (
	"slices"
	"testing"
)

func TestAppServeArgsSerialisesInstances(t *testing.T) {
	args := appServeArgs("x86_64-test", appConfig{Network: "none"}, true)

	if !slices.Contains(args, "--max-instance-concurrent-reuse-count=1") {
		t.Errorf("wasmtime args do not limit concurrent instance reuse: %v", args)
	}
}

func TestEpochMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		epoch  bool
		ok     bool
	}{
		{"needs epoch", "Caused by:\n    Module was compiled with epoch interruption but it is not enabled for the host", true, true},
		{"needs no epoch", "Caused by:\n    Module was compiled without epoch interruption but it is enabled for the host", false, true},
		{"unrelated", "wasmtime exited before it reported a serve address", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			epoch, ok := epochMismatch(tc.output)
			if ok != tc.ok || epoch != tc.epoch {
				t.Errorf("epochMismatch() = %t, %t; want %t, %t", epoch, ok, tc.epoch, tc.ok)
			}
		})
	}
}
