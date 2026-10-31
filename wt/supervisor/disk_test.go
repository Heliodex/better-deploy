package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUntrustedDiskQuotaStopsApp(t *testing.T) {
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	s.cfg.stateDir = t.TempDir()
	a := &app{name: "demo", cfg: appConfig{DiskMB: 1}}
	s.apps["demo"] = a

	dataDir := filepath.Join(s.cfg.stateDir, "demo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "blob"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	if !s.enforceDiskQuota(a) {
		t.Fatal("expected the quota check to stop the app")
	}
	a.mu.Lock()
	retired, lastError, timer := a.retired, a.lastError, a.timer
	a.mu.Unlock()
	if !retired {
		t.Error("over-quota app was not retired")
	}
	if !strings.Contains(lastError, "disk quota exceeded") {
		t.Errorf("lastError = %q, want a disk quota message", lastError)
	}
	if timer != nil {
		t.Error("over-quota app kept a restart timer")
	}
}

func TestTrustedDiskQuotaStaysSoft(t *testing.T) {
	s := testSupervisor()
	s.cfg.stateDir = t.TempDir()
	a := &app{name: "demo", cfg: appConfig{DiskMB: 1}}
	s.apps["demo"] = a

	dataDir := filepath.Join(s.cfg.stateDir, "demo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "blob"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.enforceDiskQuota(a) {
		t.Error("trusted mode must not stop an app for disk usage")
	}
	a.mu.Lock()
	retired := a.retired
	a.mu.Unlock()
	if retired {
		t.Error("trusted app was retired")
	}
}

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
