package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func atomicRollbackSupervisor(t *testing.T) (*supervisor, *storageServer, string) {
	t.Helper()
	s := newLifecycleSupervisor(t)
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.close)
	s.storage = storage
	s.backups = newBackupManager(t.TempDir(), storage, s.cfg.stateDir)
	s.deployments = newDeploymentManager(t.TempDir())
	return s, storage, storage.tokenFor("demo")
}

func TestAtomicRollbackSwapsCodeDataAndDatabase(t *testing.T) {
	s, storage, token := atomicRollbackSupervisor(t)
	a := spawnTestApp(t, s, "demo")
	createItems(t, storage, token)

	dataFile := filepath.Join(s.cfg.stateDir, "demo", "data.txt")
	if err := os.WriteFile(dataFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.backups.create(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.deployments.record("demo", a.dir)
	if err != nil {
		t.Fatal(err)
	}

	// A newer code and data state that the coordinated rollback must replace.
	if err := os.WriteFile(filepath.Join(a.dir, "main.cwasm"), []byte("newer code"), 0o644); err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "two")
	if err := os.WriteFile(dataFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/rollback/"+deployment.ID+"?backup="+snapshot.Name, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleRollback(rec, req, "demo", deployment.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("atomic rollback = %d: %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() {
		if next := s.lookup("demo"); next != nil {
			s.stop(next)
		}
	})

	code, err := os.ReadFile(filepath.Join(s.cfg.appsDir, "demo", "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(code) != "cwasm" {
		t.Errorf("code = %q, want the recorded bundle", code)
	}
	body, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old" {
		t.Errorf("data = %q, want old", body)
	}
	token = storage.tokenFor("demo")
	if got := storageCount(t, storage, token); got != 1 {
		t.Errorf("database rows = %d, want 1", got)
	}
	if next := s.lookup("demo"); next == nil || next.cmd == nil {
		t.Error("app is not running after the rollback")
	}
	overview, err := s.backups.overview("demo")
	if err != nil {
		t.Fatal(err)
	}
	if overview.Current != snapshot.Name {
		t.Errorf("backup current = %q, want %q", overview.Current, snapshot.Name)
	}
	if entries, err := os.ReadDir(filepath.Join(s.cfg.stateDir, rollbackJournalDir)); err == nil && len(entries) != 0 {
		t.Errorf("rollback journal left behind: %v", entries)
	}
}

func TestAtomicRollbackCompensatesWhenNewCodeFails(t *testing.T) {
	s, storage, token := atomicRollbackSupervisor(t)
	a := spawnTestApp(t, s, "demo")
	createItems(t, storage, token)
	dataFile := filepath.Join(s.cfg.stateDir, "demo", "data.txt")
	if err := os.WriteFile(dataFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.backups.create(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.deployments.record("demo", a.dir)
	if err != nil {
		t.Fatal(err)
	}

	// Break the retained bundle so the swapped code cannot start, then change live data and the database.
	s.cfg.wasmtimeCompiler = filepath.Join(t.TempDir(), "missing-wasmtime")
	bundle, err := s.deployments.bundle("demo", deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "main.cwasm")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "main.wasm"), []byte("wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "two")
	if err := os.WriteFile(dataFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/rollback/"+deployment.ID+"?backup="+snapshot.Name, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleRollback(rec, req, "demo", deployment.ID)
	if rec.Code < 400 {
		t.Fatalf("broken rollback = %d: %s", rec.Code, rec.Body.String())
	}

	body, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Errorf("data after compensation = %q, want new", body)
	}
	token = storage.tokenFor("demo")
	if got := storageCount(t, storage, token); got != 2 {
		t.Errorf("database rows after compensation = %d, want 2", got)
	}
	code, err := os.ReadFile(filepath.Join(s.cfg.appsDir, "demo", "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(code) != "cwasm" {
		t.Errorf("code after compensation = %q, want the previous code", code)
	}
	next := s.lookup("demo")
	if next == nil || next.cmd == nil {
		t.Error("previous app is not running after compensation")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.stateDir, rollbackJournalDir)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(s.cfg.stateDir, rollbackJournalDir))
		if len(entries) != 0 {
			t.Errorf("rollback journal left behind: %v", entries)
		}
	}
}

func TestRecoverRollbackJournalRestoresOldHalves(t *testing.T) {
	stateDir := t.TempDir()
	appsDir := filepath.Join(t.TempDir())
	appDir := filepath.Join(appsDir, "demo")
	appBackup := appDir + ".rollback-test"
	dataDir := filepath.Join(stateDir, "demo")
	dataBackup := dataDir + ".rollback-test"
	db := filepath.Join(stateDir, ".storage", "demo", "storage.db")
	dbBackup := db + ".rollback-test"
	stagedApp := filepath.Join(appsDir, ".demo.staged")
	stagedData := filepath.Join(stateDir, ".demo.staged-data")
	stagedDB := filepath.Join(stateDir, ".demo.staged.db")

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(appBackup, "main.cwasm"), "old code")
	write(filepath.Join(appDir, "main.cwasm"), "new code")
	write(filepath.Join(dataBackup, "data.txt"), "old data")
	write(filepath.Join(dataDir, "data.txt"), "new data")
	write(dbBackup, "old db")
	write(db, "new db")
	write(stagedApp, "staged")
	write(stagedData, "staged")
	write(stagedDB, "staged")

	journal := rollbackJournal{
		Name:       "demo",
		Snapshot:   "storage-20260101T000000.000000000Z.db",
		AppDir:     appDir,
		AppBackup:  appBackup,
		DataDir:    dataDir,
		DataBackup: dataBackup,
		DB:         db,
		DBBackup:   dbBackup,
		StagedApp:  stagedApp,
		StagedData: stagedData,
		StagedDB:   stagedDB,
	}
	path, err := writeRollbackJournal(stateDir, journal)
	if err != nil {
		t.Fatal(err)
	}
	_ = path

	if err := recoverRollbacks(stateDir, nil); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(appDir, "main.cwasm"): "old code",
		filepath.Join(dataDir, "data.txt"):  "old data",
		db:                                  "old db",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}
	for _, path := range []string{stagedApp, stagedData, stagedDB, appBackup, dataBackup, dbBackup} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not cleaned up: %v", path, err)
		}
	}
}

func TestRollbackJournalRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	journal := rollbackJournal{Name: "demo", Snapshot: "snapshot", AppDir: "/apps/demo"}
	path, err := writeRollbackJournal(stateDir, journal)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got rollbackJournal
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "demo" || got.Snapshot != "snapshot" || got.AppDir != "/apps/demo" {
		t.Errorf("journal = %+v", got)
	}
}
