package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clockTicks is Linux USER_HZ, which is 100 on every architecture the supervisor supports.
const clockTicks = 100

// resourceSampleInterval is how often open admin panels receive fresh CPU and memory figures.
const resourceSampleInterval = 2 * time.Second

// appStats caches the last resource sample for one app process.
type appStats struct {
	mu          sync.Mutex
	pid         int
	lastTicks   uint64
	lastSample  time.Time
	cpuPercent  float64
	memoryBytes int64
}

// sampleResources refreshes the app's CPU and memory figures from procfs. Calling it
// more than once a second reuses the previous sample so CPU deltas stay meaningful.
func (a *app) sampleResources(now time.Time) {
	a.mu.Lock()
	pid := 0
	started := a.started
	if a.cmd != nil && a.cmd.Process != nil && !a.exited {
		pid = a.cmd.Process.Pid
	}
	a.mu.Unlock()

	stats := &a.stats
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if pid == 0 {
		stats.pid = 0
		stats.lastTicks = 0
		stats.lastSample = time.Time{}
		stats.cpuPercent = 0
		stats.memoryBytes = 0
		return
	}
	if stats.pid == pid && !stats.lastSample.IsZero() && now.Sub(stats.lastSample) < time.Second {
		return
	}

	ticks, memory, err := readProcessStats(pid)
	if err != nil {
		return
	}
	if stats.pid == pid && !stats.lastSample.IsZero() {
		stats.cpuPercent = cpuPercent(stats.lastTicks, ticks, now.Sub(stats.lastSample))
	} else if !started.IsZero() {
		// A fresh process has no previous sample, so report its average since launch.
		stats.cpuPercent = cpuPercent(0, ticks, now.Sub(started))
	}
	stats.pid = pid
	stats.lastTicks = ticks
	stats.lastSample = now
	stats.memoryBytes = memory
}

// resourceStats returns the last CPU percentage (of one core) and resident memory in bytes.
func (a *app) resourceStats() (float64, int64) {
	a.stats.mu.Lock()
	defer a.stats.mu.Unlock()
	return a.stats.cpuPercent, a.stats.memoryBytes
}

// sampleAll refreshes resource usage for every app.
func (s *supervisor) sampleAll() {
	s.mu.RLock()
	apps := make([]*app, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	s.mu.RUnlock()
	now := time.Now()
	for _, a := range apps {
		a.sampleResources(now)
	}
}

// monitorResources pushes a fresh snapshot while an admin panel is subscribed, so
// CPU and memory stay live without the panel polling.
func (s *supervisor) monitorResources(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		if s.stopping.Load() {
			return
		}
		if s.events == nil || !s.events.hasSubscribers() {
			continue
		}
		s.broadcast()
	}
}

// cpuPercent converts a CPU-tick delta into percent of one core over elapsed.
func cpuPercent(previous, current uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || current < previous {
		return 0
	}
	return float64(current-previous) / clockTicks / elapsed.Seconds() * 100
}

// readProcessStats reads CPU ticks and resident memory for a process from procfs.
func readProcessStats(pid int) (uint64, int64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	ticks, err := parseProcStat(stat)
	if err != nil {
		return 0, 0, err
	}
	statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, 0, err
	}
	memory, err := parseProcStatm(statm)
	if err != nil {
		return 0, 0, err
	}
	return ticks, memory, nil
}

// parseProcStat returns utime+stime from a /proc/<pid>/stat line. The command name
// can contain spaces and parentheses, so fields are counted after the last ')'.
func parseProcStat(data []byte) (uint64, error) {
	close := bytes.LastIndexByte(data, ')')
	if close < 0 || close+2 >= len(data) {
		return 0, fmt.Errorf("malformed /proc stat")
	}
	fields := strings.Fields(string(data[close+1:]))
	// fields[0] is the process state (field 3), so utime and stime are fields 14 and 15.
	if len(fields) < 13 {
		return 0, fmt.Errorf("short /proc stat")
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

// parseProcStatm returns resident memory in bytes from a /proc/<pid>/statm line.
func parseProcStatm(data []byte) (int64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("short /proc statm")
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}
