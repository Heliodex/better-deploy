package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBackupManager(t *testing.T) (*storageServer, *backupManager) {
	t.Helper()
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.close)
	return storage, newBackupManager(t.TempDir(), storage, t.TempDir())
}

// storageCount returns the number of rows in the test table.
func storageCount(t *testing.T, storage *storageServer, token string) int {
	t.Helper()
	resp := storageSQL(t, storage, token, "/v1/query", map[string]any{"sql": `select count(*) from items`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count: %d %s", resp.StatusCode, readBody(t, resp))
	}
	result := storageResultOf(t, resp)
	if len(result.Rows) != 1 {
		t.Fatalf("count rows = %v", result.Rows)
	}
	return int(result.Rows[0][0].(float64))
}

func insertItem(t *testing.T, storage *storageServer, token, value string) {
	t.Helper()
	resp := storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql":    `insert into items (value) values (?)`,
		"params": []any{value},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insert %q: %d %s", value, resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
}

func createItems(t *testing.T, storage *storageServer, token string) {
	t.Helper()
	resp := storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql": `create table items (value text not null)`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	insertItem(t, storage, token, "one")
}

func TestBackupCreateAndRestore(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)

	info, err := backups.create(context.Background(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size == 0 {
		t.Error("backup is empty")
	}
	if _, ok := backupTime(info.Name); !ok {
		t.Errorf("backup name %q does not carry a timestamp", info.Name)
	}

	infos, err := backups.list("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != info.Name {
		t.Fatalf("list = %+v", infos)
	}

	resp := storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql": `insert into items (value) values ('two')`,
	})
	resp.Body.Close()
	if got := storageCount(t, storage, token); got != 2 {
		t.Fatalf("count before restore = %d, want 2", got)
	}

	if err := backups.restore(context.Background(), "notes", info.Name); err != nil {
		t.Fatal(err)
	}
	if got := storageCount(t, storage, token); got != 1 {
		t.Errorf("count after restore = %d, want 1", got)
	}
}

func TestBackupRejectsUnchanged(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	ctx := context.Background()

	first, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backups.create(ctx, "notes"); !errors.Is(err, errBackupUnchanged) {
		t.Fatalf("unchanged create = %v, want unchanged", err)
	}
	view, err := backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Backups) != 1 || view.Current != first.Name {
		t.Errorf("unchanged overview = %+v", view)
	}

	insertItem(t, storage, token, "two")
	second, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if second.Parent != first.Name {
		t.Errorf("second parent = %q, want %q", second.Parent, first.Name)
	}
}

func TestBackupTreeBranches(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	ctx := context.Background()

	first, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "two")
	second, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if second.Parent != first.Name {
		t.Errorf("second parent = %q, want %q", second.Parent, first.Name)
	}

	view, err := backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != second.Name {
		t.Errorf("current = %q, want %q", view.Current, second.Name)
	}

	if err := backups.restore(ctx, "notes", first.Name); err != nil {
		t.Fatal(err)
	}
	view, err = backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != first.Name {
		t.Errorf("current after restore = %q, want %q", view.Current, first.Name)
	}
	if _, err := backups.create(ctx, "notes"); !errors.Is(err, errBackupUnchanged) {
		t.Fatalf("unchanged create after restore = %v, want unchanged", err)
	}

	insertItem(t, storage, token, "three")
	third, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if third.Parent != first.Name {
		t.Errorf("third parent = %q, want %q (a new branch)", third.Parent, first.Name)
	}
	view, err = backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != third.Name {
		t.Errorf("current after branch = %q, want %q", view.Current, third.Name)
	}
	for _, backup := range view.Backups {
		if backup.Name == second.Name && backup.Parent != first.Name {
			t.Errorf("abandoned branch parent = %q, want %q", backup.Parent, first.Name)
		}
	}
}

func TestBackupTreePruneKeepsCurrentBranch(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	ctx := context.Background()

	first, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "two")
	if _, err := backups.create(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := backups.restore(ctx, "notes", first.Name); err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "three")
	branch, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}

	if err := backups.saveSettings("notes", backupSettings{Enabled: true, Every: "24h", Keep: 1}); err != nil {
		t.Fatal(err)
	}
	view, err := backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Backups) != 1 || view.Backups[0].Name != branch.Name {
		t.Fatalf("kept backups = %+v, want only %q", view.Backups, branch.Name)
	}
	if view.Current != branch.Name {
		t.Errorf("current = %q, want %q", view.Current, branch.Name)
	}
	if view.Backups[0].Parent != "" {
		t.Errorf("promoted root parent = %q, want empty", view.Backups[0].Parent)
	}
}

func TestBackupTreeRebuildsLegacyChain(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	ctx := context.Background()

	first, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	insertItem(t, storage, token, "two")
	second, err := backups.create(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backups.treePath("notes")); err != nil {
		t.Fatal(err)
	}

	view, err := backups.overview("notes")
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != second.Name {
		t.Errorf("current = %q, want %q", view.Current, second.Name)
	}
	for _, backup := range view.Backups {
		switch backup.Name {
		case first.Name:
			if backup.Parent != "" {
				t.Errorf("first parent = %q, want empty", backup.Parent)
			}
		case second.Name:
			if backup.Parent != first.Name {
				t.Errorf("second parent = %q, want %q", backup.Parent, first.Name)
			}
		}
	}
}

func TestBackupSettings(t *testing.T) {
	_, backups := testBackupManager(t)

	settings, err := backups.settings("notes")
	if err != nil {
		t.Fatal(err)
	}
	want := defaultBackupSettings()
	if settings != want {
		t.Errorf("default settings = %+v, want %+v", settings, want)
	}

	custom := backupSettings{Enabled: false, Every: "6h", Keep: 3}
	if err := backups.saveSettings("notes", custom); err != nil {
		t.Fatal(err)
	}
	settings, err = backups.settings("notes")
	if err != nil {
		t.Fatal(err)
	}
	if settings != custom {
		t.Errorf("saved settings = %+v, want %+v", settings, custom)
	}

	for _, invalid := range []backupSettings{
		{Enabled: true, Every: "soon", Keep: 3},
		{Enabled: true, Every: "0s", Keep: 3},
		{Enabled: true, Every: "24h", Keep: 0},
		{Enabled: true, Every: "24h", Keep: maxBackupKeep + 1},
	} {
		if err := backups.saveSettings("notes", invalid); err == nil {
			t.Errorf("saved invalid settings %+v", invalid)
		}
	}
}

func TestBackupPrune(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)
	if err := backups.saveSettings("notes", backupSettings{Enabled: true, Every: "1h", Keep: 2}); err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{"two", "three", "four"} {
		insertItem(t, storage, token, value)
		if _, err := backups.create(context.Background(), "notes"); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := backups.list("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Errorf("retained %d backups, want 2", len(infos))
	}
}

func TestBackupDue(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)

	now := time.Now().UTC()
	if err := backups.backupIfDue(context.Background(), "notes", now); err != nil {
		t.Fatal(err)
	}
	infos, err := backups.list("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("first due run created %d backups, want 1", len(infos))
	}
	if infos[0].Kind != backupKindAutomatic {
		t.Errorf("automatic snapshot kind = %q, want %q", infos[0].Kind, backupKindAutomatic)
	}

	insertItem(t, storage, token, "two")
	if err := backups.backupIfDue(context.Background(), "notes", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	infos, _ = backups.list("notes")
	if len(infos) != 1 {
		t.Errorf("early run created a backup: %d backups", len(infos))
	}

	if err := backups.backupIfDue(context.Background(), "notes", now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	infos, _ = backups.list("notes")
	if len(infos) != 2 {
		t.Errorf("later run left %d backups, want 2", len(infos))
	}

	if err := backups.backupIfDue(context.Background(), "notes", now.Add(49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	infos, _ = backups.list("notes")
	if len(infos) != 2 {
		t.Errorf("unchanged due run created a backup: %d backups", len(infos))
	}

	if err := backups.saveSettings("notes", backupSettings{Enabled: false, Every: "24h", Keep: 7}); err != nil {
		t.Fatal(err)
	}
	if err := backups.backupIfDue(context.Background(), "notes", now.Add(100*time.Hour)); err != nil {
		t.Fatal(err)
	}
	infos, _ = backups.list("notes")
	if len(infos) != 2 {
		t.Errorf("disabled policy created a backup: %d backups", len(infos))
	}
}

func TestBackupRestoreRejectsBadNames(t *testing.T) {
	_, backups := testBackupManager(t)
	for _, name := range []string{"", "../storage-20260101T000000.000000000Z.db", "config.json", "storage-nope.db"} {
		if err := backups.restore(context.Background(), "notes", name); !errors.Is(err, errInvalidBackupName) {
			t.Errorf("restore(%q) = %v, want invalid name", name, err)
		}
	}
	valid, err := validBackupName("storage-20260101T000000.000000000Z.db")
	if err != nil || valid == "" {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := backups.restore(context.Background(), "notes", valid); !errors.Is(err, errBackupNotFound) {
		t.Errorf("missing backup = %v, want not found", err)
	}
}

func TestHandleBackupRoutes(t *testing.T) {
	storage, backups := testBackupManager(t)
	s := testSupervisor()
	s.storage = storage
	s.backups = backups
	s.apps["notes"] = &app{name: "notes"}
	token := storage.tokenFor("notes")

	do := func(method, target, body string) (*httptest.ResponseRecorder, backupsResponse) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		var result backupsResponse
		if rec.Code == http.StatusOK {
			if err := json.UnmarshalRead(rec.Body, &result); err != nil {
				t.Fatalf("%s %s: %v", method, target, err)
			}
		}
		return rec, result
	}

	rec, result := do(http.MethodGet, "/apps/notes/backups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if result.Settings != defaultBackupSettings() || len(result.Backups) != 0 || result.Database {
		t.Errorf("initial overview = %+v", result)
	}

	rec, result = do(http.MethodPost, "/apps/notes/backups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("data-only backup = %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Backups) != 1 || result.Database || !result.Backups[0].HasData || result.Backups[0].Size != 0 {
		t.Fatalf("data-only backup = %+v", result)
	}
	dataOnly := result.Backups[0]

	createItems(t, storage, token)
	rec, result = do(http.MethodPost, "/apps/notes/backups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Backups) != 2 || !result.Database || result.Current != result.Backups[0].Name {
		t.Fatalf("after create = %+v", result)
	}
	first := result.Backups[0]
	backup := first.Name
	if result.Created != backup {
		t.Errorf("created = %q, want %q", result.Created, backup)
	}
	if first.Parent != dataOnly.Name || first.Kind != backupKindManual {
		t.Errorf("first database backup = %+v, want a manual child of the data-only snapshot", first)
	}

	rec, result = do(http.MethodPost, "/apps/notes/backups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unchanged create = %d %s", rec.Code, rec.Body.String())
	}
	if result.Created != "" || len(result.Backups) != 2 || result.Current != backup {
		t.Errorf("unchanged create = %+v, want no new snapshot", result)
	}

	rec, result = do(http.MethodPut, "/apps/notes/backups/config", `{"enabled":false,"every":"6h","keep":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("config = %d %s", rec.Code, rec.Body.String())
	}
	if result.Settings.Enabled || result.Settings.Every != "6h" || result.Settings.Keep != 3 {
		t.Errorf("saved settings = %+v", result.Settings)
	}

	rec, _ = do(http.MethodPut, "/apps/notes/backups/config", `{"enabled":true,"every":"24h","keep":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid config = %d, want 400", rec.Code)
	}

	resp := storageSQL(t, storage, token, "/v1/exec", map[string]any{
		"sql": `insert into items (value) values ('two')`,
	})
	resp.Body.Close()
	rec, result = do(http.MethodPost, "/apps/notes/backups/"+backup+"/restore", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	if result.Current != backup {
		t.Errorf("current after restore = %q, want %q", result.Current, backup)
	}
	if got := storageCount(t, storage, token); got != 1 {
		t.Errorf("count after restore = %d, want 1", got)
	}

	rec, _ = do(http.MethodPost, "/apps/notes/backups/../config/restore", "")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("traversal name = %d, want 400 or 404", rec.Code)
	}
}

func TestBackupManagerUsesBackupDirOutsideStorage(t *testing.T) {
	storage, backups := testBackupManager(t)
	if filepath.Dir(backups.dir) == storage.dir {
		t.Errorf("backups dir %q sits inside storage dir %q", backups.dir, storage.dir)
	}
}

func TestBackupIncludesData(t *testing.T) {
	storage, backups := testBackupManager(t)
	token := storage.tokenFor("notes")
	createItems(t, storage, token)

	dataFile := filepath.Join(backups.dataDir, "notes", "file.txt")
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := backups.create(context.Background(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasData || info.DataSize == 0 {
		t.Errorf("snapshot data = %+v, want a data archive", info)
	}
	infos, err := backups.list("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || !infos[0].HasData {
		t.Errorf("list = %+v, want data fields", infos)
	}

	if err := os.WriteFile(dataFile, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := backups.create(context.Background(), "notes"); err != nil {
		t.Fatalf("changed data should create a snapshot: %v", err)
	}

	temp, hasData, err := backups.stageData("notes", info.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !hasData {
		t.Fatal("snapshot has no data archive")
	}
	defer os.RemoveAll(temp)
	if err := backups.installData("notes", temp); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one" {
		t.Errorf("restored data = %q, want %q", data, "one")
	}
	if err := backups.saveSettings("notes", backupSettings{Enabled: true, Every: "24h", Keep: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backups.dataPath("notes", info.Name)); !os.IsNotExist(err) {
		t.Errorf("pruned snapshot data archive still exists: %v", err)
	}
}

func TestBackupDataOnlySnapshot(t *testing.T) {
	_, backups := testBackupManager(t)
	dataDir := filepath.Join(backups.dataDir, "plain")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "notes.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := backups.create(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasData || first.DataSize == 0 || first.Size != 0 {
		t.Errorf("snapshot = %+v, want a data-only snapshot", first)
	}
	if _, err := os.Stat(filepath.Join(backups.appDir("plain"), first.Name)); !os.IsNotExist(err) {
		t.Errorf("data-only snapshot should not have a database file: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dataDir, "notes.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := backups.create(context.Background(), "plain"); err != nil {
		t.Fatal(err)
	}

	temp, hasData, err := backups.stageData("plain", first.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !hasData {
		t.Fatal("data-only snapshot should stage a data archive")
	}
	defer os.RemoveAll(temp)
	body, err := os.ReadFile(filepath.Join(temp, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one" {
		t.Errorf("staged data = %q, want one", body)
	}

	if err := backups.restore(context.Background(), "plain", first.Name); err != nil {
		t.Fatal(err)
	}
	if err := backups.installData("plain", temp); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(dataDir, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one" {
		t.Errorf("restored data = %q, want one", body)
	}
}

func TestBackupSchedulerIncludesDataOnlyApps(t *testing.T) {
	_, backups := testBackupManager(t)
	dataDir := filepath.Join(backups.dataDir, "plain")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state.txt"), []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}

	backups.runDue(context.Background(), time.Now())
	infos, err := backups.list("plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("automatic snapshots = %d, want 1 for an app without a database", len(infos))
	}
	if infos[0].Kind != backupKindAutomatic || !infos[0].HasData {
		t.Errorf("snapshot = %+v, want an automatic data snapshot", infos[0])
	}
}

func TestRollbackWithBackupRestoresData(t *testing.T) {
	s := newLifecycleSupervisor(t)
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.close)
	s.storage = storage
	s.backups = newBackupManager(t.TempDir(), storage, s.cfg.stateDir)
	s.deployments = newDeploymentManager(t.TempDir())

	a := spawnTestApp(t, s, "demo")
	dataFile := filepath.Join(s.cfg.stateDir, "demo", "data.txt")
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.backups.create(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Size != 0 {
		t.Fatalf("snapshot = %+v, want a data-only snapshot", snapshot)
	}
	deployment, err := s.deployments.record("demo", a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/rollback/"+deployment.ID+"?backup="+snapshot.Name, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleRollback(rec, req, "demo", deployment.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback with backup = %d: %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() {
		if next := s.lookup("demo"); next != nil {
			s.stop(next)
		}
	})
	body, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep" {
		t.Errorf("restored data = %q, want keep", body)
	}
}
