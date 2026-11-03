package main

import (
	"slices"
	"testing"
)

func TestAppServeArgsSerializesInstances(t *testing.T) {
	args := appServeArgs("x86_64-test", appConfig{Network: "none"}, true)

	if !slices.Contains(args, "--max-instance-concurrent-reuse-count=1") {
		t.Errorf("wasmtime args do not limit concurrent instance reuse: %v", args)
	}
}
