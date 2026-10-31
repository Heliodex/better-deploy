package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDataQuotaConfigPatchAppliesBeforePersist(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.localhost", DiskMB: 128}, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantUntrusted); err != nil {
		t.Fatal(err)
	}
	runner := &fakeQuotaRunner{}
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	s.quota = newDataQuota(dataQuotaSetquota, "/srv/wt", dir, "setquota", "findmnt", runner.run)
	a := &app{name: "demo", dir: dir, uid: 20006, cfg: cfg}
	s.apps[a.name] = a
	s.hosts[cfg.Host] = a

	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"disk_mb":512}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	want := []quotaCall{{bin: "setquota", args: []string{"-u", "20006", "524288", "524288", strconv.FormatInt(maxAppEntries, 10), strconv.FormatInt(maxAppEntries, 10), "/srv/wt"}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("commands = %+v\nwant %+v", runner.calls, want)
	}
	saved, err := readAppConfig(dir, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DiskMB != 512 {
		t.Errorf("saved disk_mb = %d, want 512", saved.DiskMB)
	}
}

func TestDataQuotaConfigPatchFailureKeepsConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.localhost", DiskMB: 128}, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantUntrusted); err != nil {
		t.Fatal(err)
	}
	runner := &fakeQuotaRunner{respond: func(bin string, _ []string) (string, error) {
		if bin == "setquota" {
			return "", errors.New("setquota: permission denied")
		}
		return "", nil
	}}
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	s.quota = newDataQuota(dataQuotaSetquota, "/srv/wt", dir, "setquota", "findmnt", runner.run)
	a := &app{name: "demo", dir: dir, uid: 20007, cfg: cfg}
	s.apps[a.name] = a
	s.hosts[cfg.Host] = a

	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"disk_mb":512}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	saved, err := readAppConfig(dir, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DiskMB != 128 {
		t.Errorf("saved disk_mb = %d, want the old 128", saved.DiskMB)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"], "permission denied") {
		t.Errorf("error = %q", body["error"])
	}
}

func TestDataQuotaConfigPatchWithoutUIDIsDeferred(t *testing.T) {
	dir := t.TempDir()
	cfg, err := normalizeAppConfig(appConfig{Name: "demo", Host: "demo.localhost", DiskMB: 128}, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAppConfig(dir, cfg, tenantUntrusted); err != nil {
		t.Fatal(err)
	}
	runner := &fakeQuotaRunner{}
	s := testSupervisor()
	s.cfg.tenantMode = tenantUntrusted
	s.quota = newDataQuota(dataQuotaSetquota, "/srv/wt", dir, "setquota", "findmnt", runner.run)
	a := &app{name: "demo", dir: dir, cfg: cfg}
	s.apps[a.name] = a
	s.hosts[cfg.Host] = a

	req := httptest.NewRequest(http.MethodPatch, "/apps/demo/config", strings.NewReader(`{"disk_mb":512}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("a never-launched app should defer its quota: %+v", runner.calls)
	}
	saved, err := readAppConfig(dir, "demo", "localhost", tenantUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DiskMB != 512 {
		t.Errorf("saved disk_mb = %d, want 512", saved.DiskMB)
	}
}

func TestClearAppDataQuotaUsesAppOrAllocatedUID(t *testing.T) {
	runner := &fakeQuotaRunner{}
	uids, err := newUIDAllocator(filepath.Join(t.TempDir(), ".uids.json"), 20000)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := uids.Allocate("demo")
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{
		cfg:   config{stateDir: t.TempDir()},
		uids:  uids,
		quota: newDataQuota(dataQuotaSetquota, "/srv/wt", t.TempDir(), "setquota", "findmnt", runner.run),
	}
	s.clearAppDataQuota("demo", &app{name: "demo"})
	want := []quotaCall{{bin: "setquota", args: []string{"-u", strconv.Itoa(uid), "0", "0", "0", "0", "/srv/wt"}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("commands = %+v\nwant %+v", runner.calls, want)
	}
}

func TestDataQuotaClearFailureDoesNotPanic(t *testing.T) {
	runner := &fakeQuotaRunner{respond: func(string, []string) (string, error) {
		return "", errors.New("setquota: offline")
	}}
	s := &supervisor{quota: newDataQuota(dataQuotaSetquota, "/srv/wt", t.TempDir(), "setquota", "findmnt", runner.run)}
	s.clearAppDataQuota("demo", &app{name: "demo", uid: 20008})
	if len(runner.calls) != 1 {
		t.Errorf("clear ran %d commands, want one best-effort attempt", len(runner.calls))
	}
}
