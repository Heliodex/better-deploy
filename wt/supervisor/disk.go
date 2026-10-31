package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const diskUsageInterval = 10 * time.Second

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

func (s *supervisor) appDataBytes(name string) (int64, error) {
	var total int64
	if err := walkSize(filepath.Join(s.cfg.stateDir, name), &total); err != nil {
		return total, err
	}

	storageDir := filepath.Join(s.cfg.stateDir, ".storage", name)
	entries, err := os.ReadDir(storageDir)
	if os.IsNotExist(err) {
		return total, nil
	}
	if err != nil {
		return total, err
	}
	for _, entry := range entries {
		// storage.db, storage.db-wal and storage.db-shm are the database; changes.log is not part of the quota.
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "storage.db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return total, err
		}
		total += info.Size()
	}
	return total, nil
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
	if limit <= 0 {
		return false
	}
	bytes, err := s.appDataBytes(a.name)
	if err != nil {
		log.Printf("app %s: disk quota check: %v", a.name, err)
		return false
	}
	if bytes <= limit {
		return false
	}

	message := fmt.Sprintf("disk quota exceeded: %d bytes of %d bytes used; app stopped", bytes, limit)
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
