package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testSecretStore(tb testing.TB) *secretStore {
	tb.Helper()

	store, err := newSecretStore(filepath.Join(tb.TempDir(), secretDirName))
	if err != nil {
		tb.Fatal(err)
	}

	return store
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func secretTestSupervisor(tb testing.TB) (*supervisor, *app) {
	tb.Helper()

	s := testSupervisor()
	s.secrets = testSecretStore(tb)
	s.cfg.stateDir = tb.TempDir()
	dir := tb.TempDir()
	a := &app{name: "demo", dir: dir, cfg: appConfig{Name: "demo", Host: "demo.localhost"}}
	s.apps[a.name] = a
	s.hosts[a.cfg.Host] = a

	return s, a
}

func TestSecretStorePersistenceAndPermissions(t *testing.T) {
	store := testSecretStore(t)
	if err := store.set("demo", "API_KEY", "first-value"); err != nil {
		t.Fatal(err)
	}

	if value, ok := store.get("demo", "API_KEY"); !ok || value != "first-value" {
		t.Errorf("get = %q, %v", value, ok)
	}

	if keys := store.keys("demo"); len(keys) != 1 || keys[0] != "API_KEY" {
		t.Errorf("keys = %v", keys)
	}

	if values := store.values("demo"); values["API_KEY"] != "first-value" {
		t.Errorf("values = %v", values)
	}

	dirInfo, err := os.Stat(store.dir)
	if err != nil {
		t.Fatal(err)
	}

	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("secret dir mode = %o, want 700", dirInfo.Mode().Perm())
	}

	fileInfo, err := os.Stat(store.file("demo"))
	if err != nil {
		t.Fatal(err)
	}

	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %o, want 600", fileInfo.Mode().Perm())
	}

	reloaded, err := newSecretStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}

	if value, ok := reloaded.get("demo", "API_KEY"); !ok || value != "first-value" {
		t.Errorf("reloaded get = %q, %v", value, ok)
	}

	if err := reloaded.delete("demo", "API_KEY"); err != nil {
		t.Fatal(err)
	}

	if _, ok := reloaded.get("demo", "API_KEY"); ok {
		t.Error("deleted secret is still present")
	}

	reloadedAgain, err := newSecretStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := reloadedAgain.get("demo", "API_KEY"); ok {
		t.Error("deleted secret survived a reload")
	}

	if err := reloadedAgain.set("demo", "OTHER", "value"); err != nil {
		t.Fatal(err)
	}

	if err := reloadedAgain.remove("demo"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(store.file("demo")); !os.IsNotExist(err) {
		t.Errorf("secret file still exists after remove: %v", err)
	}
}

func TestSecretStoreRejectsInvalidNamesKeysAndValues(t *testing.T) {
	store := testSecretStore(t)
	for _, key := range []string{"", "1KEY", "BAD-KEY", "has space", "A=B", strings.Repeat("A", maxSecretKeyBytes+1)} {
		if err := store.set("demo", key, "value"); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}

	if err := store.set("Demo", "KEY", "value"); err == nil {
		t.Error("invalid app name was accepted")
	}

	if err := store.set("demo", "KEY", strings.Repeat("x", maxSecretValueBytes+1)); err == nil {
		t.Error("oversized value was accepted")
	}

	if err := store.set("demo", "KEY", "a\x00b"); err == nil {
		t.Error("value with NUL was accepted")
	}
}

func TestSecretStoreFailsOnCorruptState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), secretDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "demo.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := newSecretStore(dir); err == nil {
		t.Fatal("corrupt secret state did not fail startup")
	}
}

func TestSecretRotationKeepsOldValuesForRedaction(t *testing.T) {
	store := testSecretStore(t)
	if err := store.set("demo", "TOKEN", "old-token"); err != nil {
		t.Fatal(err)
	}

	if err := store.set("demo", "TOKEN", "new-token"); err != nil {
		t.Fatal(err)
	}

	values := store.redactionValues("demo")
	if !containsString(values, "old-token") || !containsString(values, "new-token") {
		t.Errorf("redaction values = %v, want both rotations", values)
	}

	if err := store.delete("demo", "TOKEN"); err != nil {
		t.Fatal(err)
	}

	values = store.redactionValues("demo")
	if !containsString(values, "old-token") || !containsString(values, "new-token") {
		t.Errorf("redaction values after delete = %v, want history", values)
	}
}

func TestAppLogRedactsSecretsInAppAndSupervisorLines(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.close()

	logs.setSecrets([]string{"short", "longer-secret"})
	messages := logs.subscribe()

	if _, err := logs.Write([]byte("app says longer-secret and short\n")); err != nil {
		t.Fatal(err)
	}

	entry := <-messages
	if strings.Contains(entry.Message, "longer-secret") || strings.Contains(entry.Message, "short") {
		t.Errorf("streamed app line = %q", entry.Message)
	}

	if entry.Message != "app says [redacted] and [redacted]" {
		t.Errorf("streamed app line = %q", entry.Message)
	}

	logs.logSupervisor("GET /?token=longer-secret 200")

	entry = <-messages
	if strings.Contains(entry.Message, "longer-secret") {
		t.Errorf("streamed supervisor line = %q", entry.Message)
	}

	entries, err := logs.tail(10)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.Contains(entry.Message, "longer-secret") || strings.Contains(entry.Message, "short") {
			t.Errorf("tail line leaked a secret: %q", entry.Message)
		}
	}

	for _, path := range []string{logs.path(), logs.supervisorPath()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(data), "longer-secret") || strings.Contains(string(data), "short") {
			t.Errorf("%s leaked a secret: %s", filepath.Base(path), data)
		}
	}
}

func TestAppInfoRedactsLastError(t *testing.T) {
	s, a := secretTestSupervisor(t)
	if err := s.secrets.set("demo", "API_KEY", "super-secret-value"); err != nil {
		t.Fatal(err)
	}

	a.lastError = "failed with super-secret-value"

	info := s.info(a)
	if strings.Contains(info.LastError, "super-secret-value") {
		t.Errorf("LastError leaked a secret: %q", info.LastError)
	}

	if info.LastError != "failed with [redacted]" {
		t.Errorf("LastError = %q", info.LastError)
	}
}
