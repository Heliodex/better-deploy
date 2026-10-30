package main

import (
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
