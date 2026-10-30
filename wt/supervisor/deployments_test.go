package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeDeploymentSource(t *testing.T, host string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm "+host), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"`+host+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDeploymentHistoryRecordListAndPrune(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")
	var recorded deploymentInfo
	for range deploymentKeep + 2 {
		info, err := manager.record("demo", source)
		if err != nil {
			t.Fatal(err)
		}
		recorded = info
	}

	infos, err := manager.list("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != deploymentKeep {
		t.Fatalf("history length = %d, want %d", len(infos), deploymentKeep)
	}
	if infos[0].ID != recorded.ID {
		t.Errorf("newest ID = %q, want %q", infos[0].ID, recorded.ID)
	}
	for i := 1; i < len(infos); i++ {
		if infos[i-1].ID <= infos[i].ID {
			t.Errorf("history is not newest first: %q then %q", infos[i-1].ID, infos[i].ID)
		}
	}
	dir, err := manager.bundle("demo", infos[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.json", "main.cwasm", deploymentMetaName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := manager.bundle("demo", "nope"); err == nil {
		t.Error("invalid deployment id was accepted")
	}
}

func TestRollbackInstallsPreviousBundle(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "demo")
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))
	info, err := s.deployments.record("demo", a.dir)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/demo/rollback/"+info.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleRollback(rec, req, "demo", info.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d: %s", rec.Code, rec.Body.String())
	}
	next := s.lookup("demo")
	if next == nil || next == a {
		t.Fatalf("rollback did not replace the app: %v", next)
	}
	t.Cleanup(func() { s.stop(next) })
	infos, err := s.deployments.list("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Errorf("history length after rollback = %d, want 2", len(infos))
	}
}
