package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	diskUsageInterval  = 10 * time.Second
	diskBudgetInterval = time.Minute
)

// maxAppEntries bounds inode use as well as bytes. It is a variable so tests can use a small limit.
var maxAppEntries int64 = 100_000

type diskUsageCache struct {
	mu    sync.Mutex
	bytes int64
	at    time.Time
}

func walkSize(root string, total *int64) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			*total += info.Size()
		}
		return nil
	})
}

// appDataUsage measures /data plus the app's SQLite database files, counting every non-root directory entry
// so empty files and directories still count against the inode quota.
func (s *supervisor) appDataUsage(name string) (bytes, entries int64, err error) {
	dataDir := filepath.Join(s.cfg.stateDir, name)
	if err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path != dataDir {
			entries++
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	}); err != nil {
		return bytes, entries, err
	}

	storageDir := filepath.Join(s.cfg.stateDir, ".storage", name)
	storageEntries, err := os.ReadDir(storageDir)
	if os.IsNotExist(err) {
		return bytes, entries, nil
	}
	if err != nil {
		return bytes, entries, err
	}
	for _, entry := range storageEntries {
		// storage.db, storage.db-wal and storage.db-shm are the database; changes.log is not part of the quota.
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "storage.db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return bytes, entries, err
		}
		bytes += info.Size()
		entries++
	}
	return bytes, entries, nil
}

// appDataBytes keeps the existing byte-only callers and report.
func (s *supervisor) appDataBytes(name string) (int64, error) {
	bytes, _, err := s.appDataUsage(name)
	return bytes, err
}

// diskBytes returns cached app-data usage: /data plus the app's SQLite database files, excluding logs, backups and deployment history.
func (s *supervisor) diskBytes(a *app) int64 {
	now := time.Now()
	a.disk.mu.Lock()
	if !a.disk.at.IsZero() && now.Sub(a.disk.at) < diskUsageInterval {
		bytes := a.disk.bytes
		a.disk.mu.Unlock()
		return bytes
	}
	a.disk.mu.Unlock()

	bytes, err := s.appDataBytes(a.name)
	if err != nil {
		log.Printf("app %s: disk usage: %v", a.name, err)
	}
	a.disk.mu.Lock()
	a.disk.bytes = bytes
	a.disk.at = now
	a.disk.mu.Unlock()
	return bytes
}

// enforceDiskQuota stops an untrusted app whose measured /data and database usage exceeds disk_mb.
// Stopping retires the app, so the process watcher does not restart it and the reason stays in lastError.
func (s *supervisor) enforceDiskQuota(a *app) bool {
	if a == nil || !s.cfg.untrusted() {
		return false
	}
	a.mu.Lock()
	limit := int64(a.cfg.DiskMB) << 20
	a.mu.Unlock()
	if limit <= 0 && maxAppEntries <= 0 {
		return false
	}
	bytes, entries, err := s.appDataUsage(a.name)
	if err != nil {
		log.Printf("app %s: disk quota check: %v", a.name, err)
		return false
	}

	var message string
	switch {
	case limit > 0 && bytes > limit:
		message = fmt.Sprintf("disk quota exceeded: %d bytes of %d bytes used; app stopped", bytes, limit)
	case maxAppEntries > 0 && entries > maxAppEntries:
		message = fmt.Sprintf("file count quota exceeded: %d entries of %d allowed; app stopped", entries, maxAppEntries)
	default:
		return false
	}

	a.mu.Lock()
	a.lastError = message
	a.mu.Unlock()
	log.Printf("app %s: %s", a.name, message)
	s.cancelRestart(a)
	s.drainAndStop(a, "disk quota")
	s.broadcast()
	return true
}

// monitorDiskQuota checks each app's measured usage on the same cadence as the usage cache.
func (s *supervisor) monitorDiskQuota() {
	ticker := time.NewTicker(diskUsageInterval)
	defer ticker.Stop()
	for range ticker.C {
		if s.stopping.Load() {
			return
		}
		for _, a := range s.allApps() {
			s.enforceDiskQuota(a)
		}
	}
}

// diskTotals is the measured size of the apps and state trees, without counting a tree twice when one contains the other.
type diskTotals struct {
	appsBytes, stateBytes, totalBytes int64
}

// treeBytes sums the regular files under root. A missing tree is empty rather than an error.
func treeBytes(root string) (int64, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	if err := walkSize(root, &total); err != nil {
		return 0, err
	}
	return total, nil
}

// pathWithin reports whether inner is outer itself or lies below it.
func pathWithin(inner, outer string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// diskTotals measures WT_APPS_DIR and WT_STATE_DIR. When one is nested in the other, only the containing tree is counted.
func (s *supervisor) diskTotals() (diskTotals, error) {
	apps, err := treeBytes(s.cfg.appsDir)
	if err != nil {
		return diskTotals{}, fmt.Errorf("apps directory: %w", err)
	}
	state, err := treeBytes(s.cfg.stateDir)
	if err != nil {
		return diskTotals{}, fmt.Errorf("state directory: %w", err)
	}

	totals := diskTotals{appsBytes: apps, stateBytes: state}
	switch {
	case pathWithin(s.cfg.stateDir, s.cfg.appsDir):
		totals.totalBytes = apps
	case pathWithin(s.cfg.appsDir, s.cfg.stateDir):
		totals.totalBytes = state
	default:
		totals.totalBytes = apps + state
	}
	return totals, nil
}

// overBudget reports whether measured usage is above the global budget. Zero means unlimited.
func (s *supervisor) overBudget() bool {
	if s.cfg.diskBudgetBytes <= 0 {
		return false
	}
	totals, err := s.diskTotals()
	if err != nil {
		log.Printf("disk budget: %v", err)
		return false
	}
	return totals.totalBytes > s.cfg.diskBudgetBytes
}

// rejectOverBudget writes 507 and reports true when the global budget is already exceeded.
// It measures once, and fails closed with 500 when usage cannot be measured.
func (s *supervisor) rejectOverBudget(w http.ResponseWriter) bool {
	if s.cfg.diskBudgetBytes <= 0 {
		return false
	}
	totals, err := s.diskTotals()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "measure disk usage: "+err.Error())
		return true
	}
	if totals.totalBytes <= s.cfg.diskBudgetBytes {
		return false
	}
	writeError(w, http.StatusInsufficientStorage, fmt.Sprintf(
		"global disk budget exceeded: %d of %d bytes used; free space or raise WT_DISK_BUDGET_MB",
		totals.totalBytes, s.cfg.diskBudgetBytes))
	return true
}

// diskUsageResponse is the authenticated GET /usage body. budgetBytes is zero when unlimited.
type diskUsageResponse struct {
	AppsBytes   int64 `json:"appsBytes"`
	StateBytes  int64 `json:"stateBytes"`
	TotalBytes  int64 `json:"totalBytes"`
	BudgetBytes int64 `json:"budgetBytes"`
	OverBudget  bool  `json:"overBudget"`
}

func (s *supervisor) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	totals, err := s.diskTotals()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "measure disk usage: "+err.Error())
		return
	}
	budget := s.cfg.diskBudgetBytes
	writeJSON(w, http.StatusOK, diskUsageResponse{
		AppsBytes:   totals.appsBytes,
		StateBytes:  totals.stateBytes,
		TotalBytes:  totals.totalBytes,
		BudgetBytes: budget,
		OverBudget:  budget > 0 && totals.totalBytes > budget,
	})
}

// monitorDiskBudget logs crossing the global budget without deleting data or stopping apps.
func (s *supervisor) monitorDiskBudget() {
	ticker := time.NewTicker(diskBudgetInterval)
	defer ticker.Stop()
	over := false
	for range ticker.C {
		if s.stopping.Load() {
			return
		}
		now := s.overBudget()
		switch {
		case now && !over:
			if totals, err := s.diskTotals(); err == nil {
				log.Printf("warning: disk budget exceeded: %d of %d bytes used", totals.totalBytes, s.cfg.diskBudgetBytes)
			} else {
				log.Printf("warning: disk budget exceeded")
			}
		case !now && over:
			log.Printf("disk budget usage is back under the limit")
		}
		over = now
	}
}
