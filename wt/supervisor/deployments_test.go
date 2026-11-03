package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	for name, body := range map[string]string{
		"main.wasm":     "wasm",
		"artifact.json": `{"wasmtime":"wasmtime-cli 50.0.0","target":"x86_64-test"}`,
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

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

	for _, name := range []string{"app.json", "main.cwasm", "main.wasm", "artifact.json", deploymentMetaName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	if !recorded.Rebuildable || recorded.Runtime != "wasmtime-cli 50.0.0" || recorded.Target != "x86_64-test" {
		t.Errorf("recorded = %+v, want a rebuildable bundle with its artifact metadata", recorded)
	}

	if recorded.Digest != digestText("wasm") || recorded.WasmDigest != recorded.Digest || recorded.WasmBytes != int64(len("wasm")) {
		t.Errorf("recorded = %+v, want the wasm digest %s and its size", recorded, digestText("wasm"))
	}

	if _, err := manager.bundle("demo", "nope"); err == nil {
		t.Error("invalid deployment id was accepted")
	}
}

func TestDeploymentHistoryRecordsFailure(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	digest := digestText("wasm")

	info, err := manager.recordFailure("demo", digest, "new version did not start: boom")
	if err != nil {
		t.Fatal(err)
	}

	if info.Status != deploymentStatusFail || info.Error != "new version did not start: boom" || info.Digest != digest {
		t.Errorf("failure record = %+v", info)
	}

	infos, err := manager.list("demo")
	if err != nil {
		t.Fatal(err)
	}

	if len(infos) != 1 || infos[0].Status != deploymentStatusFail || infos[0].Error == "" {
		t.Fatalf("failure list = %+v", infos)
	}

	if _, err := manager.bundle("demo", info.ID); !errors.Is(err, errDeploymentNotFound) {
		t.Errorf("failed deploy bundle = %v, want not found", err)
	}
}

func TestDeploymentHistoryMarksRunningDigest(t *testing.T) {
	s := testSupervisor()
	s.deployments = newDeploymentManager(t.TempDir())

	first, err := s.deployments.record("demo", writeDeploymentSource(t, "first.localhost"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.deployments.record("demo", writeDeploymentSource(t, "second.localhost"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.deployments.recordFailure("demo", second.Digest, "boom"); err != nil {
		t.Fatal(err)
	}

	s.apps["demo"] = &app{name: "demo", digest: second.Digest}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/deployments", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleDeployments(rec, req, "demo")

	if rec.Code != http.StatusOK {
		t.Fatalf("deployments = %d: %s", rec.Code, rec.Body.String())
	}

	var list []deploymentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	if len(list) != 3 {
		t.Fatalf("history length = %d, want 3", len(list))
	}

	if list[0].Status != deploymentStatusFail || list[0].Current {
		t.Errorf("newest failure = %+v, want no current mark", list[0])
	}

	if !list[1].Current || list[1].Digest != second.Digest {
		t.Errorf("current record = %+v, want %s", list[1], second.Digest)
	}

	if list[2].Current || list[2].Digest != first.Digest {
		t.Errorf("older record = %+v, want the first digest", list[2])
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

func TestDeploymentHistoryLegacyCwasmDigest(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")

	recorded, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	if recorded.Digest != digestText("cwasm demo.localhost") || recorded.WasmDigest != "" || recorded.WasmBytes != 0 {
		t.Errorf("legacy record = %+v, want the cwasm digest and no wasm metadata", recorded)
	}
}

func TestDeploymentHistorySharesArtifacts(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())

	source := writeDeploymentSource(t, "demo.localhost")
	if err := os.WriteFile(filepath.Join(source, "main.cwasm"), bytes.Repeat([]byte("c"), 10<<10), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(source, "main.wasm"), bytes.Repeat([]byte("w"), 10<<10), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	firstDir, err := manager.bundle("demo", first.ID)
	if err != nil {
		t.Fatal(err)
	}

	secondDir, err := manager.bundle("demo", second.ID)
	if err != nil {
		t.Fatal(err)
	}

	firstInfo, err := os.Stat(filepath.Join(firstDir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}

	secondInfo, err := os.Stat(filepath.Join(secondDir, "main.cwasm"))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(firstInfo, secondInfo) {
		t.Error("identical artifacts were copied instead of shared")
	}

	blobs, err := os.ReadDir(filepath.Join(manager.appDir("demo"), deploymentBlobDir))
	if err != nil {
		t.Fatal(err)
	}

	if len(blobs) != 2 {
		t.Errorf("blob count = %d, want 2 (cwasm and wasm)", len(blobs))
	}

	used, err := manager.usage("demo")
	if err != nil {
		t.Fatal(err)
	}

	if used >= first.Bytes+second.Bytes {
		t.Errorf("usage = %d, want less than the logical total %d", used, first.Bytes+second.Bytes)
	}
}

func TestDeploymentHistoryPrunesUnreferencedBlobs(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())

	source := writeDeploymentSource(t, "demo.localhost")
	if err := os.WriteFile(filepath.Join(source, "main.wasm"), []byte("wasm"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := range deploymentKeep + 2 {
		if err := os.WriteFile(filepath.Join(source, "main.cwasm"), fmt.Appendf(nil, "cwasm %d", i), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := manager.record("demo", source); err != nil {
			t.Fatal(err)
		}
	}

	blobs, err := os.ReadDir(filepath.Join(manager.appDir("demo"), deploymentBlobDir))
	if err != nil {
		t.Fatal(err)
	}
	// One shared wasm blob plus one cwasm per retained bundle.
	if len(blobs) != deploymentKeep+1 {
		t.Errorf("blob count = %d, want %d", len(blobs), deploymentKeep+1)
	}
}

func TestDeploymentHistoryByteBudget(t *testing.T) {
	previous := deploymentMaxBytes

	deploymentMaxBytes = 16
	defer func() { deploymentMaxBytes = previous }()

	manager := newDeploymentManager(t.TempDir())

	source := writeDeploymentSource(t, "demo.localhost")
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(source, "main.cwasm"), fmt.Appendf(nil, "cwasm %d", i), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := manager.record("demo", source); err != nil {
			t.Fatal(err)
		}
	}

	infos, err := manager.list("demo")
	if err != nil {
		t.Fatal(err)
	}

	if len(infos) != 1 {
		t.Errorf("history length = %d, want 1 under a tiny byte budget", len(infos))
	}
}

func writeStaticFile(t *testing.T, source, rel, body string) {
	t.Helper()

	path := filepath.Join(source, "static", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentHistorySharesStatic(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")
	body := bytes.Repeat([]byte("s"), 8<<10)
	writeStaticFile(t, source, "_app/immutable/app.js", string(body))

	first, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	rel := "_app/immutable/app.js"
	if first.Static[rel] == "" {
		t.Fatalf("static metadata = %v, want the file digest", first.Static)
	}

	blobsBefore, err := os.ReadDir(filepath.Join(manager.appDir("demo"), deploymentBlobDir))
	if err != nil {
		t.Fatal(err)
	}

	before, err := manager.usage("demo")
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	blobsAfter, err := os.ReadDir(filepath.Join(manager.appDir("demo"), deploymentBlobDir))
	if err != nil {
		t.Fatal(err)
	}

	if len(blobsAfter) != len(blobsBefore) {
		t.Errorf("blob count = %d, want %d: identical static files added blobs", len(blobsAfter), len(blobsBefore))
	}

	after, err := manager.usage("demo")
	if err != nil {
		t.Fatal(err)
	}

	if after-before >= int64(len(body)) {
		t.Errorf("usage grew by %d bytes, want the shared static tree counted once", after-before)
	}

	firstDir, err := manager.bundle("demo", first.ID)
	if err != nil {
		t.Fatal(err)
	}

	secondDir, err := manager.bundle("demo", second.ID)
	if err != nil {
		t.Fatal(err)
	}

	firstInfo, err := os.Stat(filepath.Join(firstDir, "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	secondInfo, err := os.Stat(filepath.Join(secondDir, "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(firstInfo, secondInfo) {
		t.Error("identical static files were copied instead of shared")
	}

	blobInfo, err := os.Stat(filepath.Join(manager.blobDir("demo"), first.Static[rel]))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(firstInfo, blobInfo) {
		t.Error("the bundle's static file is not hardline to its blob")
	}
}

func TestDeploymentHistoryPrunesStaticBlobs(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")
	writeStaticFile(t, source, "_app/immutable/app.js", "console.log('app')")

	first, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	rel := "_app/immutable/app.js"
	blob := filepath.Join(manager.blobDir("demo"), first.Static[rel])

	if err := os.RemoveAll(filepath.Join(manager.appDir("demo"), first.ID)); err != nil {
		t.Fatal(err)
	}

	if err := manager.prune("demo"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(blob); err != nil {
		t.Errorf("shared static blob was removed while another bundle references it: %v", err)
	}

	secondDir, err := manager.bundle("demo", second.ID)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := os.ReadFile(filepath.Join(secondDir, "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	if string(stored) != "console.log('app')" {
		t.Errorf("static file = %q", stored)
	}

	if err := os.RemoveAll(secondDir); err != nil {
		t.Fatal(err)
	}

	if err := manager.prune("demo"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("unreferenced static blob still exists: %v", err)
	}
}

func TestDeploymentHistoryChangedStaticGetsNewBlob(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")
	rel := "_app/immutable/app.js"
	writeStaticFile(t, source, rel, "one")

	first, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	writeStaticFile(t, source, rel, "two")

	second, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	if first.Static[rel] == second.Static[rel] {
		t.Fatalf("digests are equal: %q", first.Static[rel])
	}

	blobs, err := os.ReadDir(filepath.Join(manager.appDir("demo"), deploymentBlobDir))
	if err != nil {
		t.Fatal(err)
	}
	// One shared cwasm plus one blob per static version.
	if len(blobs) != 3 {
		t.Errorf("blob count = %d, want 3", len(blobs))
	}

	firstDir, err := manager.bundle("demo", first.ID)
	if err != nil {
		t.Fatal(err)
	}

	secondDir, err := manager.bundle("demo", second.ID)
	if err != nil {
		t.Fatal(err)
	}

	oldBody, err := os.ReadFile(filepath.Join(firstDir, "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	newBody, err := os.ReadFile(filepath.Join(secondDir, "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	if string(oldBody) != "one" || string(newBody) != "two" {
		t.Errorf("static bodies = %q and %q", oldBody, newBody)
	}
}

func TestDeploymentHistoryLegacyStaticWithoutMetadata(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	id := time.Now().UTC().Format(deploymentTimeLayout)

	dir := filepath.Join(manager.appDir("demo"), id)
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"app.json":      `{"name":"demo","host":"demo.localhost"}`,
		"main.cwasm":    "cwasm demo.localhost",
		"static/app.js": "console.log('legacy')",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	meta := fmt.Sprintf(`{"id":%q,"deployedAt":%q,"digest":"legacy","size":19,"rebuildable":false,"bytes":61}`+"\n", id, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, deploymentMetaName), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := manager.list("demo")
	if err != nil {
		t.Fatal(err)
	}

	if len(infos) != 1 || infos[0].Static != nil {
		t.Fatalf("legacy list = %+v, want one bundle without static metadata", infos)
	}

	bundle, err := manager.bundle("demo", id)
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := copyBundle(bundle, dst); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dst, "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "console.log('legacy')" {
		t.Errorf("copied static file = %q", body)
	}

	if err := manager.prune("demo"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("legacy bundle was pruned: %v", err)
	}
}

func TestDeploymentHistoryKeepsStaticFiles(t *testing.T) {
	manager := newDeploymentManager(t.TempDir())
	source := writeDeploymentSource(t, "demo.localhost")

	static := filepath.Join(source, "static", "_app", "immutable", "app.js")
	if err := os.MkdirAll(filepath.Dir(static), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(static, []byte("console.log('app')"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := manager.record("demo", source)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := manager.bundle("demo", info.ID)
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "static", "_app", "immutable", "app.js"))
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "console.log('app')" {
		t.Errorf("static file = %q", body)
	}

	if info.Bytes == 0 {
		t.Error("bundle bytes do not include static files")
	}
}

func TestDeploymentInfoEndpoint(t *testing.T) {
	s := testSupervisor()
	s.deployments = newDeploymentManager(t.TempDir())

	recorded, err := s.deployments.record("demo", writeDeploymentSource(t, "demo.localhost"))
	if err != nil {
		t.Fatal(err)
	}

	s.apps["demo"] = &app{name: "demo", digest: recorded.Digest}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/deployments/"+recorded.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("deployment info = %d: %s", rec.Code, rec.Body.String())
	}

	var info deploymentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}

	if info.ID != recorded.ID || !info.Current || info.Status != deploymentStatusOK {
		t.Errorf("deployment info = %+v", info)
	}
}
