package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppDiskUsageAndQuota(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	a := &app{name: "demo", cfg: appConfig{DiskMB: 1}}
	s.apps["demo"] = a

	data := make([]byte, 600<<10)
	dataDir := filepath.Join(s.cfg.stateDir, "demo")
	storageDir := filepath.Join(s.cfg.stateDir, ".storage", "demo")
	for _, dir := range []string{dataDir, storageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "blob"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "storage.db"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "changes.log"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	bytes, err := s.appDataBytes("demo")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(2 * 600 << 10); bytes != want {
		t.Errorf("app data bytes = %d, want %d (changes.log must be excluded)", bytes, want)
	}
	info := s.info(a)
	if info.DiskBytes != bytes {
		t.Errorf("info disk bytes = %d, want %d", info.DiskBytes, bytes)
	}
	if info.DiskLimitBytes != 1<<20 || !info.OverQuota {
		t.Errorf("quota = %d over = %v, want 1 MiB and over", info.DiskLimitBytes, info.OverQuota)
	}
}
