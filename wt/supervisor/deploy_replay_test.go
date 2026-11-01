package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeploymentReplayPersists(t *testing.T) {
	dir := t.TempDir()
	m := newDeploymentManager(dir)
	entry := deployReplayEntry{
		Status: http.StatusOK,
		Header: http.Header{"Etag": {`"etag"`}},
		Body:   []byte("deployed"),
		At:     time.Now().UTC(),
	}
	if err := m.storeReplay("demo", "key", entry); err != nil {
		t.Fatal(err)
	}

	restarted := newDeploymentManager(dir)
	got, ok := restarted.replay("demo", "key", time.Now())
	if !ok {
		t.Fatal("replay did not find the persisted entry")
	}
	if got.Status != entry.Status || string(got.Body) != "deployed" || got.Header.Get("ETag") != `"etag"` {
		t.Errorf("replay = %+v", got)
	}
	info, err := os.Stat(restarted.replayPath("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestDeploymentReplayExpires(t *testing.T) {
	m := newDeploymentManager(t.TempDir())
	old := deployReplayEntry{Status: http.StatusOK, At: time.Now().Add(-deploymentReplayTTL - time.Minute)}
	if err := m.storeReplay("demo", "old", old); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.replay("demo", "old", time.Now()); ok {
		t.Error("an expired entry was replayed")
	}

	fresh := deployReplayEntry{Status: http.StatusOK, At: time.Now()}
	if err := m.storeReplay("demo", "fresh", fresh); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.replay("demo", "fresh", time.Now()); !ok {
		t.Error("a fresh entry was not replayed")
	}
}

func TestDeploymentReplayCapsEntries(t *testing.T) {
	m := newDeploymentManager(t.TempDir())
	base := time.Now().Add(-time.Hour)
	for i := range deploymentReplayKeep + 5 {
		entry := deployReplayEntry{Status: http.StatusOK, At: base.Add(time.Duration(i) * time.Second)}
		if err := m.storeReplay("demo", fmt.Sprintf("key-%d", i), entry); err != nil {
			t.Fatal(err)
		}
	}
	file, err := m.readReplayFile("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != deploymentReplayKeep {
		t.Errorf("entries = %d, want %d", len(file.Entries), deploymentReplayKeep)
	}
	for _, key := range []string{"key-0", "key-1", "key-2", "key-3", "key-4"} {
		if _, ok := file.Entries[key]; ok {
			t.Errorf("old entry %s was not pruned", key)
		}
	}
	if _, ok := file.Entries[fmt.Sprintf("key-%d", deploymentReplayKeep+4)]; !ok {
		t.Error("the newest entry was pruned")
	}
}

func TestDeploymentReplayCorruptFileDoesNotBlockStore(t *testing.T) {
	m := newDeploymentManager(t.TempDir())
	if err := os.MkdirAll(m.appDir("demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.replayPath("demo"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.replay("demo", "key", time.Now()); ok {
		t.Error("a corrupt file produced a replay")
	}
	entry := deployReplayEntry{Status: http.StatusOK, At: time.Now()}
	if err := m.storeReplay("demo", "key", entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.replay("demo", "key", time.Now()); !ok {
		t.Error("storing over a corrupt file did not produce a replayable entry")
	}
}

func TestDeploymentReplayClear(t *testing.T) {
	m := newDeploymentManager(t.TempDir())
	entry := deployReplayEntry{Status: http.StatusOK, At: time.Now()}
	if err := m.storeReplay("demo", "key", entry); err != nil {
		t.Fatal(err)
	}
	if err := m.clearReplay("demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.replay("demo", "key", time.Now()); ok {
		t.Error("a cleared entry was replayed")
	}
	if err := m.clearReplay("demo"); err != nil {
		t.Errorf("clearing a missing file = %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.appDir("demo"), deploymentReplayName)); !os.IsNotExist(err) {
		t.Errorf("idempotency file still exists: %v", err)
	}
}
