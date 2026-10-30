package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// resourcePersistInterval is how often chart histories are written to disk. A crash loses at most this much history.
	resourcePersistInterval = 10 * time.Second
)

// clockTicks is Linux USER_HZ, which is 100 on every architecture the supervisor supports.
const clockTicks = 100

const (
	// resourceSampleInterval is how often the supervisor reads CPU and memory and how often open panels receive them.
	resourceSampleInterval = time.Second
	// resourceHistoryWindow is how much history the panel charts; at the sample interval this is 1800 points.
	resourceHistoryWindow = 30 * time.Minute
	resourceHistoryPoints = int(resourceHistoryWindow / resourceSampleInterval)
)

// resourceSample is one point in an app's CPU and memory history.
type resourceSample struct {
	At          time.Time `json:"at"`
	CPUPercent  float64   `json:"cpuPercent"`
	MemoryBytes int64     `json:"memoryBytes"`
}

// appStats caches the last resource sample for one app process and keeps a short history for the charts.
type appStats struct {
	mu                    sync.Mutex
	pid                   int
	lastTicks             uint64
	lastSample, sampledAt time.Time
	cpuPercent            float64
	memoryBytes           int64
	lastAppend            time.Time
	history               []resourceSample
}

// appendHistory records a point when the chart interval has passed. The caller holds stats.mu.
func (s *appStats) appendHistory(now time.Time, cpu float64, memory int64) {
	if !s.lastAppend.IsZero() && now.Sub(s.lastAppend) < resourceSampleInterval/2 {
		return
	}
	s.lastAppend = now
	s.history = append(s.history, resourceSample{At: now.UTC(), CPUPercent: cpu, MemoryBytes: memory})
	if len(s.history) > resourceHistoryPoints {
		s.history = s.history[len(s.history)-resourceHistoryPoints:]
	}
}

// recordHistory appends the current figures with the sample timestamp, so the panel
// can deduplicate history fetched from the API against snapshots arriving over SSE.
func (a *app) recordHistory() {
	a.stats.mu.Lock()
	defer a.stats.mu.Unlock()
	if a.stats.sampledAt.IsZero() || time.Since(a.stats.sampledAt) > 2*resourceSampleInterval {
		return
	}
	a.stats.appendHistory(a.stats.sampledAt, a.stats.cpuPercent, a.stats.memoryBytes)
}

// resourceStats returns the last CPU percentage (of one core), resident memory in bytes, and when it was sampled.
func (a *app) resourceStats() (float64, int64, time.Time) {
	a.stats.mu.Lock()
	defer a.stats.mu.Unlock()
	return a.stats.cpuPercent, a.stats.memoryBytes, a.stats.sampledAt
}

// resourceHistory returns a copy of the app's recent samples, oldest first.
func (a *app) resourceHistory() []resourceSample {
	a.stats.mu.Lock()
	defer a.stats.mu.Unlock()
	out := make([]resourceSample, len(a.stats.history))
	copy(out, a.stats.history)
	return out
}

// recordAll appends one history point per app; the monitor calls it after sampling.
func (s *supervisor) recordAll() {
	for _, a := range s.allApps() {
		a.recordHistory()
	}
}

type resourceHistoryResponse struct {
	Samples []resourceSample `json:"samples"`
}

// resourcesDir holds one JSON history file per app, outside any app's /data preopen.
func (s *supervisor) resourcesDir() string {
	return filepath.Join(s.cfg.stateDir, ".resources")
}

func (s *supervisor) resourceHistoryPath(name string) string {
	return filepath.Join(s.resourcesDir(), name+".json")
}

// persistResources writes each app's chart history to disk so it survives restarts.
// Failures are logged and ignored: history is best-effort.
func (s *supervisor) persistResources() {
	if s.cfg.stateDir == "" {
		return
	}
	if err := os.MkdirAll(s.resourcesDir(), 0o755); err != nil {
		log.Printf("resources: persist: %v", err)
		return
	}
	for _, a := range s.allApps() {
		data, err := json.Marshal(resourceHistoryResponse{Samples: a.resourceHistory()})
		if err != nil {
			continue
		}
		tmp, err := os.CreateTemp(s.resourcesDir(), "."+a.name+".*.json")
		if err != nil {
			continue
		}
		tmpName := tmp.Name()
		_, writeErr := tmp.Write(append(data, '\n'))
		closeErr := tmp.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(tmpName)
			continue
		}
		if err := os.Rename(tmpName, s.resourceHistoryPath(a.name)); err != nil {
			_ = os.Remove(tmpName)
		}
	}
}

// loadResourceHistory reads a persisted history back into a fresh app, trimming to
// the chart window. A missing or corrupt file means empty history.
func (s *supervisor) loadResourceHistory(a *app) {
	data, err := os.ReadFile(s.resourceHistoryPath(a.name))
	if err != nil {
		return
	}
	var out resourceHistoryResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return
	}
	cutoff := time.Now().Add(-resourceHistoryWindow)
	kept := make([]resourceSample, 0, len(out.Samples))
	for _, sample := range out.Samples {
		if sample.At.IsZero() || sample.At.Before(cutoff) {
			continue
		}
		kept = append(kept, sample)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].At.Before(kept[j].At) })
	if len(kept) > resourceHistoryPoints {
		kept = kept[len(kept)-resourceHistoryPoints:]
	}
	if len(kept) == 0 {
		return
	}
	a.stats.mu.Lock()
	a.stats.history = kept
	a.stats.lastAppend = kept[len(kept)-1].At
	a.stats.mu.Unlock()
}

// cpuPercent converts a CPU-tick delta into percent of one core over elapsed.
func cpuPercent(previous, current uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || current < previous {
		return 0
	}
	return float64(current-previous) / clockTicks / elapsed.Seconds() * 100
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

// sampleResources refreshes the app's CPU and memory figures from procfs. Calling it
// more than twice per sample interval reuses the previous sample so CPU deltas stay meaningful.
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
		stats.sampledAt = now.UTC()
		stats.cpuPercent = 0
		stats.memoryBytes = 0
		return
	}
	if stats.pid == pid && !stats.lastSample.IsZero() && now.Sub(stats.lastSample) < resourceSampleInterval/2 {
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
	stats.sampledAt = now.UTC()
	stats.memoryBytes = memory
}

// handleResources returns the app's CPU and memory history for the charts.
func (s *supervisor) handleResources(w http.ResponseWriter, _ *http.Request, a *app) {
	a.sampleResources(time.Now())
	writeJSON(w, http.StatusOK, resourceHistoryResponse{Samples: a.resourceHistory()})
}

// sampleAll refreshes resource usage for every app.
func (s *supervisor) sampleAll() {
	apps := s.allApps()
	now := time.Now()
	for _, a := range apps {
		a.sampleResources(now)
	}
}

// monitorResources samples every app in the background so the charts have history
// before a panel opens, and pushes a fresh snapshot while one is subscribed.
func (s *supervisor) monitorResources(every time.Duration) {
	s.sampleAll()
	s.recordAll()
	s.persistResources()
	lastPersist := time.Now()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		if s.stopping.Load() {
			return
		}
		s.sampleAll()
		s.recordAll()
		if time.Since(lastPersist) >= resourcePersistInterval {
			s.persistResources()
			lastPersist = time.Now()
		}
		if s.events != nil && s.events.hasSubscribers() {
			s.broadcast()
		}
	}
}
