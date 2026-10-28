package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	// Fields after the closing parenthesis start at the state field, so utime and stime are fields 14 and 15.
	line := []byte("1234 (wasmtime) S 1 1234 1234 0 -1 4194560 100 0 0 0 12 34 0 0 20 0 1 0 12345 100000 100 18446744073709551615")
	ticks, err := parseProcStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 46 {
		t.Errorf("ticks = %d, want 46", ticks)
	}
}

func TestParseProcStatHandlesParensInCommand(t *testing.T) {
	line := []byte("1234 (wasm)time) S 1 1234 1234 0 -1 4194560 100 0 0 0 7 8 0 0 20 0 1 0 12345 100000 100 18446744073709551615")
	ticks, err := parseProcStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 15 {
		t.Errorf("ticks = %d, want 15", ticks)
	}
}

func TestParseProcStatRejectsMalformedLines(t *testing.T) {
	for _, line := range [][]byte{
		nil,
		[]byte("1234 (wasmtime S 1 2 3"),
		[]byte("1234 (wasmtime) S 1 2 3 4 5 6 7 8 9 10 11"),
		[]byte("1234 (wasmtime) S 1 1234 1234 0 -1 4194560 100 0 0 0 12 nope 0 0 20"),
	} {
		if _, err := parseProcStat(line); err == nil {
			t.Errorf("expected an error for %q", line)
		}
	}
}

func TestParseProcStatm(t *testing.T) {
	memory, err := parseProcStatm([]byte("1000 250 0 0 0 0 0"))
	if err != nil {
		t.Fatal(err)
	}
	want := int64(250 * os.Getpagesize())
	if memory != want {
		t.Errorf("memory = %d, want %d", memory, want)
	}
	if _, err := parseProcStatm([]byte("1000")); err == nil {
		t.Error("expected an error for a short statm line")
	}
	if _, err := parseProcStatm([]byte("1000 nope")); err == nil {
		t.Error("expected an error for a non-numeric resident page count")
	}
}

func TestCPUPercent(t *testing.T) {
	cases := []struct {
		previous, current uint64
		elapsed           time.Duration
		want              float64
	}{
		{0, 100, 10 * time.Second, 10},
		{100, 100, time.Second, 0},
		{100, 50, time.Second, 0},
		{0, 100, 0, 0},
		{0, 100, time.Second, 100},
	}
	for _, tc := range cases {
		if got := cpuPercent(tc.previous, tc.current, tc.elapsed); got != tc.want {
			t.Errorf("cpuPercent(%d, %d, %s) = %v, want %v", tc.previous, tc.current, tc.elapsed, got, tc.want)
		}
	}
}

func TestSampleResourcesReadsSelf(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("procfs is unavailable")
	}
	app := &app{started: time.Now().Add(-2 * time.Second)}
	app.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	app.sampleResources(time.Now())

	cpu, memory := app.resourceStats()
	if memory <= 0 {
		t.Errorf("memory = %d, want a positive RSS", memory)
	}
	if cpu < 0 {
		t.Errorf("cpu = %v, want a non-negative percentage", cpu)
	}
}

func TestSampleResourcesClearsStoppedApp(t *testing.T) {
	app := &app{}
	app.stats.cpuPercent = 42
	app.stats.memoryBytes = 1 << 20
	app.sampleResources(time.Now())
	cpu, memory := app.resourceStats()
	if cpu != 0 || memory != 0 {
		t.Errorf("stopped app stats = %v, %d; want 0, 0", cpu, memory)
	}
}
