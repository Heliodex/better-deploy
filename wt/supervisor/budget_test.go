package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiskTotalsSeparateAndNested(t *testing.T) {
	t.Run("separate", func(t *testing.T) {
		root := t.TempDir()
		s := &supervisor{cfg: config{appsDir: filepath.Join(root, "apps"), stateDir: filepath.Join(root, "state")}}
		writeSizedFile(t, filepath.Join(s.cfg.appsDir, "app", "code.wasm"), 100)
		writeSizedFile(t, filepath.Join(s.cfg.stateDir, "app", "data"), 200)

		totals, err := s.diskTotals()
		if err != nil {
			t.Fatal(err)
		}
		if totals.appsBytes != 100 || totals.stateBytes != 200 || totals.totalBytes != 300 {
			t.Errorf("totals = %+v, want apps 100, state 200, total 300", totals)
		}
	})

	t.Run("apps inside state", func(t *testing.T) {
		root := t.TempDir()
		state := filepath.Join(root, "state")
		s := &supervisor{cfg: config{appsDir: filepath.Join(state, "apps"), stateDir: state}}
		writeSizedFile(t, filepath.Join(s.cfg.appsDir, "app", "code.wasm"), 100)
		writeSizedFile(t, filepath.Join(s.cfg.stateDir, "logs", "app.log"), 200)

		totals, err := s.diskTotals()
		if err != nil {
			t.Fatal(err)
		}
		if totals.appsBytes != 100 || totals.stateBytes != 300 || totals.totalBytes != 300 {
			t.Errorf("totals = %+v, want apps 100, state 300, total 300", totals)
		}
	})

	t.Run("state inside apps", func(t *testing.T) {
		root := t.TempDir()
		apps := filepath.Join(root, "apps")
		s := &supervisor{cfg: config{appsDir: apps, stateDir: filepath.Join(apps, "state")}}
		writeSizedFile(t, filepath.Join(s.cfg.appsDir, "app", "code.wasm"), 100)
		writeSizedFile(t, filepath.Join(s.cfg.stateDir, "app", "data"), 200)

		totals, err := s.diskTotals()
		if err != nil {
			t.Fatal(err)
		}
		if totals.appsBytes != 300 || totals.stateBytes != 200 || totals.totalBytes != 300 {
			t.Errorf("totals = %+v, want apps 300, state 200, total 300", totals)
		}
	})
}

func TestUsageEndpointReportsBudget(t *testing.T) {
	root := t.TempDir()
	s := testSupervisor()
	s.cfg.appsDir = filepath.Join(root, "apps")
	s.cfg.stateDir = filepath.Join(root, "state")
	s.cfg.diskBudgetBytes = 250
	writeSizedFile(t, filepath.Join(s.cfg.appsDir, "app", "code.wasm"), 100)
	writeSizedFile(t, filepath.Join(s.cfg.stateDir, "app", "data"), 200)

	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.adminMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status = %d: %s", rec.Code, rec.Body.String())
	}

	var usage diskUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.AppsBytes != 100 || usage.StateBytes != 200 || usage.TotalBytes != 300 ||
		usage.BudgetBytes != 250 || !usage.OverBudget {
		t.Errorf("usage = %+v, want apps 100, state 200, total 300, budget 250, over budget", usage)
	}
}

func TestDeployRejectedOverBudget(t *testing.T) {
	root := t.TempDir()
	s := testSupervisor()
	s.cfg.appsDir = filepath.Join(root, "apps")
	s.cfg.stateDir = filepath.Join(root, "state")
	s.cfg.diskBudgetBytes = 10
	s.deployments = newDeploymentManager(t.TempDir())
	writeSizedFile(t, filepath.Join(s.cfg.appsDir, "existing", "code.wasm"), 100)

	req := httptest.NewRequest(http.MethodPost, "/apps/demo/deployments", nil)
	rec := httptest.NewRecorder()
	s.handleDeploymentCreate(rec, req, "demo")
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-budget deploy status = %d, want 507: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disk budget") {
		t.Errorf("over-budget deploy message = %q, want a disk budget explanation", rec.Body.String())
	}
}
