package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretAPIIsAuthenticatedAndWriteOnly(t *testing.T) {
	s, _ := secretTestSupervisor(t)

	req := httptest.NewRequest(http.MethodGet, "/apps/demo/secrets", nil)
	rec := httptest.NewRecorder()
	s.handleApp(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/apps/demo/secrets/API_KEY", strings.NewReader(`{"value":"super-secret-value"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Errorf("PUT echoed the value: %s", rec.Body.String())
	}
	var mutation secretMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mutation); err != nil {
		t.Fatal(err)
	}
	if mutation.Key != "API_KEY" || !mutation.RestartRequired {
		t.Errorf("mutation response = %+v", mutation)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/demo/secrets", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET secrets = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Errorf("GET secrets leaked the value: %s", rec.Body.String())
	}
	var listing secretKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Keys) != 1 || listing.Keys[0] != "API_KEY" {
		t.Errorf("listing = %+v", listing)
	}

	req = httptest.NewRequest(http.MethodPut, "/apps/demo/secrets/API_KEY", strings.NewReader(`{"value":"rotated-secret"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mutation); err != nil {
		t.Fatal(err)
	}
	if !mutation.RestartRequired {
		t.Error("rotation did not require a restart")
	}
	values := s.secrets.redactionValues("demo")
	if !containsString(values, "super-secret-value") || !containsString(values, "rotated-secret") {
		t.Errorf("rotation history = %v", values)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/demo/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleAppConfig(rec, req, "demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") || strings.Contains(rec.Body.String(), "rotated-secret") {
		t.Errorf("GET config leaked a value: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"secret_keys":["API_KEY"]`) {
		t.Errorf("GET config has no secret keys: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/apps/demo/secrets/API_KEY", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.secrets.get("demo", "API_KEY"); ok {
		t.Error("DELETE did not remove the secret")
	}
}

func TestSecretAPIValidatesInput(t *testing.T) {
	s, _ := secretTestSupervisor(t)
	cases := []struct {
		body string
		key  string
	}{
		{`{"value":"x"}`, "1BAD"},
		{`{}`, "GOOD_KEY"},
		{`{"value":"a\u0000b"}`, "GOOD_KEY"},
		{`{"value":"` + strings.Repeat("x", maxSecretBodyBytes) + `"}`, "GOOD_KEY"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPut, "/apps/demo/secrets/"+tc.key, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s %s = %d, want 400", tc.key, tc.body, rec.Code)
		}
	}
}

func TestAppArgsResolvesSecretReferencesWithoutValuesInArgv(t *testing.T) {
	store := testSecretStore(t)
	s := testSupervisor()
	s.secrets = store
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.close)
	s.storage = storage
	if err := store.set("demo", "API_KEY", "super-secret-value"); err != nil {
		t.Fatal(err)
	}
	a := &app{name: "demo", cfg: appConfig{Env: map[string]string{
		"API_KEY": "secret://API_KEY",
		"LITERAL": "plain-value",
	}}}
	args, env, err := s.appArgs(a, "main.cwasm")
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(args, " ")
	if strings.Contains(argv, "super-secret-value") || strings.Contains(argv, "plain-value") {
		t.Errorf("argv contains an env value: %q", argv)
	}
	if !strings.Contains(argv, "--env API_KEY") || !strings.Contains(argv, "--env LITERAL") {
		t.Errorf("argv is missing --env KEY: %q", argv)
	}
	token := storage.tokenFor("demo")
	for _, forbidden := range []string{storage.url(), token} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("argv contains storage state %q: %q", forbidden, argv)
		}
	}

	environment := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("env entry %q has no =", entry)
		}
		environment[key] = value
	}
	if environment["API_KEY"] != "super-secret-value" || environment["LITERAL"] != "plain-value" {
		t.Errorf("env = %v", environment)
	}
	if environment["WT_STORAGE_URL"] != storage.url() || environment["WT_STORAGE_TOKEN"] != token {
		t.Errorf("storage env = %v", environment)
	}
}

func TestAppArgsMissingSecretIsClearError(t *testing.T) {
	s := testSupervisor()
	s.secrets = testSecretStore(t)
	a := &app{name: "demo", cfg: appConfig{Env: map[string]string{"API_KEY": "secret://MISSING"}}}
	if _, _, err := s.appArgs(a, "main.cwasm"); err == nil || !strings.Contains(err.Error(), `secret "MISSING"`) {
		t.Fatalf("missing secret error = %v", err)
	}
}

func TestLaunchAbortsBeforeProcessOnMissingSecret(t *testing.T) {
	s := newLifecycleSupervisor(t)
	store, err := newSecretStore(filepath.Join(s.cfg.stateDir, secretDirName))
	if err != nil {
		t.Fatal(err)
	}
	s.secrets = store
	dir := filepath.Join(s.cfg.appsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = s.spawn("demo", dir, appConfig{
		Name:   "demo",
		Host:   "demo.localhost",
		Health: "/",
		Env:    map[string]string{"API_KEY": "secret://MISSING"},
	})
	if err == nil || !strings.Contains(err.Error(), `secret "MISSING"`) {
		t.Fatalf("spawn error = %v", err)
	}
	if a := s.lookup("demo"); a != nil {
		t.Errorf("failed launch registered an app with pid %d", appPID(a))
	}
}

func TestPurgeRemovesSecretsButDeleteKeepsThem(t *testing.T) {
	s := newLifecycleSupervisor(t)
	store, err := newSecretStore(filepath.Join(s.cfg.stateDir, secretDirName))
	if err != nil {
		t.Fatal(err)
	}
	s.secrets = store
	if err := store.set("demo", "API_KEY", "super-secret-value"); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(s.cfg.appsDir, "demo")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{name: "demo", dir: appDir, cfg: appConfig{Name: "demo", Host: "demo.localhost"}}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/apps/demo", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(store.file("demo")); err != nil {
		t.Errorf("delete without purge removed secrets: %v", err)
	}

	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/apps/demo?purge=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("purge delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(store.file("demo")); !os.IsNotExist(err) {
		t.Errorf("purge kept the secret file: %v", err)
	}
}

func TestDeploymentHistoryKeepsSecretReferences(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	appJSON := `{"name":"demo","host":"demo.localhost","env":{"API_KEY":"secret://API_KEY"}}`
	if err := os.WriteFile(filepath.Join(source, "app.json"), []byte(appJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	recorded, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := manager.bundle("demo", recorded.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(retained), `secret://API_KEY`) {
		t.Errorf("retained app.json lost the reference: %s", retained)
	}
	if strings.Contains(string(retained), "super-secret-value") {
		t.Errorf("retained app.json contains a resolved secret: %s", retained)
	}
}
