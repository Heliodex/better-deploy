package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func deployArchive(t *testing.T, s *supervisor, name, archive string, query string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("artifact", "app.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/"+name+query, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleApp(rec, req)
	return rec
}

func appPID(a *app) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd == nil || a.cmd.Process == nil || a.exited {
		return 0
	}
	return a.cmd.Process.Pid
}

func TestDeployDryRunValidatesWithoutInstalling(t *testing.T) {
	archive := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm",
	})

	s := testSupervisor()
	s.cfg.appsDir = t.TempDir()
	s.cfg.domain = "localhost"
	rec := deployArchive(t, s, "demo", archive, "?dry_run=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run status = %d: %s", rec.Code, rec.Body.String())
	}

	var got deployValidationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Config.Host != "demo.localhost" {
		t.Errorf("dry run response = %+v", got)
	}
	if got.Digest != digestText("wasm") {
		t.Errorf("dry run digest = %q, want %q", got.Digest, digestText("wasm"))
	}
	if rec.Header().Get("X-WT-Wasm-Digest") != got.Digest {
		t.Errorf("dry run digest header = %q, want %q", rec.Header().Get("X-WT-Wasm-Digest"), got.Digest)
	}
	if s.lookup("demo") != nil {
		t.Error("dry run registered the app")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.appsDir, "demo")); !os.IsNotExist(err) {
		t.Errorf("dry run installed an app directory: %v", err)
	}
}

func TestDeploySameDigestKeepsProcessAndUpdatesStatic(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.domain = "localhost"
	s.cfg.wasmtime = os.Args[0]
	s.cfg.wasmtimeTarget = "x86_64-test"
	cache := t.TempDir()
	s.cfg.cacheDir = cache
	s.cfg.cacheMaxBytes = defaultCacheMaxBytes
	counter := filepath.Join(t.TempDir(), "compiles")
	s.cfg.wasmtimeCompiler = writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "compiled", counter, false)
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))
	t.Cleanup(func() {
		if a := s.lookup("demo"); a != nil {
			s.stop(a)
		}
	})

	appJSON := `{"name":"demo","host":"demo.localhost"}`
	first := writeTarGz(t, map[string]string{
		"app.json":          appJSON,
		"main.wasm":         "wasm-one",
		"static/index.html": "first",
	})
	rec := deployArchive(t, s, "demo", first, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first deploy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-WT-Deploy-Mode") != "full" {
		t.Errorf("first deploy mode = %q, want full", rec.Header().Get("X-WT-Deploy-Mode"))
	}
	digest := digestText("wasm-one")
	if rec.Header().Get("X-WT-Wasm-Digest") != digest {
		t.Errorf("first deploy digest = %q, want %q", rec.Header().Get("X-WT-Wasm-Digest"), digest)
	}
	running := s.lookup("demo")
	pid := appPID(running)
	if pid == 0 {
		t.Fatal("first deploy did not start a process")
	}
	if got := compileCount(t, counter); got != 1 {
		t.Fatalf("compile calls = %d, want 1 after the first deploy", got)
	}

	infoRec := httptest.NewRecorder()
	infoReq := httptest.NewRequest(http.MethodGet, "/apps/demo", nil)
	infoReq.Header.Set("Authorization", "Bearer secret")
	s.handleApp(infoRec, infoReq)
	if infoRec.Code != http.StatusOK {
		t.Fatalf("app info = %d: %s", infoRec.Code, infoRec.Body.String())
	}
	var info appInfo
	if err := json.Unmarshal(infoRec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.CurrentDigest != digest {
		t.Errorf("currentDigest = %q, want %q", info.CurrentDigest, digest)
	}

	second := writeTarGz(t, map[string]string{
		"app.json":          appJSON,
		"main.wasm":         "wasm-one",
		"static/index.html": "second",
	})
	rec = deployArchive(t, s, "demo", second, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("static deploy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-WT-Deploy-Mode") != "static" {
		t.Errorf("static deploy mode = %q, want static", rec.Header().Get("X-WT-Deploy-Mode"))
	}
	if got := appPID(s.lookup("demo")); got != pid {
		t.Errorf("static deploy restarted the app: pid %d, want %d", got, pid)
	}
	if got := compileCount(t, counter); got != 1 {
		t.Errorf("compile calls = %d, want a static deploy to skip compiling", got)
	}
	static, err := os.ReadFile(filepath.Join(s.cfg.appsDir, "demo", "static", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(static) != "second" {
		t.Errorf("static/index.html = %q, want the new frontend", static)
	}

	history, err := s.deployments.list("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2", len(history))
	}
	for _, record := range history {
		if record.Digest != digest || record.WasmDigest != digest || record.WasmBytes != int64(len("wasm-one")) {
			t.Errorf("history record = %+v, want wasm digest %s", record, digest)
		}
	}

	old := s.lookup("demo")
	third := writeTarGz(t, map[string]string{
		"app.json":          appJSON,
		"main.wasm":         "wasm-two",
		"static/index.html": "third",
	})
	rec = deployArchive(t, s, "demo", third, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("changed deploy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-WT-Deploy-Mode") != "full" {
		t.Errorf("changed deploy mode = %q, want full", rec.Header().Get("X-WT-Deploy-Mode"))
	}
	if got := appPID(s.lookup("demo")); got == pid || got == 0 {
		t.Errorf("changed wasm kept pid %d, want a new process", got)
	}
	if got := compileCount(t, counter); got != 2 {
		t.Errorf("compile calls = %d, want a changed wasm to compile", got)
	}
	if old.logs == nil {
		t.Fatal("old app has no log handle")
	}
	old.logs.mu.Lock()
	closed := old.logs.closed
	old.logs.mu.Unlock()
	if !closed {
		t.Error("deploy did not close the replaced app's log files")
	}
}

func TestLegacyCwasmOnlyBundleStarts(t *testing.T) {
	s := newLifecycleSupervisor(t)
	a := spawnTestApp(t, s, "legacy")
	if got := appPID(a); got == 0 {
		t.Fatal("legacy cwasm-only app is not running")
	}
	if a.digest == "" {
		t.Error("legacy app has no current digest")
	}
}
