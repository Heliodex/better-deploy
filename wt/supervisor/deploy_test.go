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
	"time"
)

func deployArchive(t *testing.T, s *supervisor, name, archive string, query string) *httptest.ResponseRecorder {
	t.Helper()
	return deployArchiveHeaders(t, s, name, archive, query, nil)
}

// deployQueued posts an archive to the queued deployment endpoint and returns its immediate response.
func deployQueued(t *testing.T, s *supervisor, name, archive string, query string, headers map[string]string) *httptest.ResponseRecorder {
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

	req := httptest.NewRequest(http.MethodPost, "/apps/"+name+"/deployments"+query, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	s.handleApp(rec, req)

	return rec
}

// deploymentID returns the id from a queued response's Location or body.
func deploymentID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	if location := rec.Header().Get("Location"); location != "" {
		return filepath.Base(location)
	}

	var info deploymentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}

	return info.ID
}

// waitDeployment polls one deployment until it reaches a terminal state.
func waitDeployment(t *testing.T, s *supervisor, name, id string) deploymentInfo {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		req := httptest.NewRequest(http.MethodGet, "/apps/"+name+"/deployments/"+id, nil)
		req.Header.Set("Authorization", "Bearer secret")

		rec := httptest.NewRecorder()
		s.handleApp(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("deployment info = %d: %s", rec.Code, rec.Body.String())
		}

		var info deploymentInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}

		if info.Status != deploymentStatusQueued && info.Status != deploymentStatusRunning {
			return info
		}

		if time.Now().After(deadline) {
			t.Fatalf("deployment %s did not finish", id)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// deployArchiveHeaders uploads through the queued resource, waits for the result and returns a sync-shaped recorder.
func deployArchiveHeaders(t *testing.T, s *supervisor, name, archive string, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	rec := deployQueued(t, s, name, archive, query, headers)
	if rec.Code != http.StatusAccepted {
		return rec
	}

	etag := rec.Header().Get("ETag")
	info := waitDeployment(t, s, name, deploymentID(t, rec))
	result := httptest.NewRecorder()

	if info.Status != deploymentStatusOK {
		data, err := json.Marshal(map[string]string{"error": info.Error})
		if err != nil {
			t.Fatal(err)
		}

		result.WriteHeader(http.StatusBadGateway)
		result.Body.Write(data)

		return result
	}

	a := s.lookup(name)
	if a == nil {
		t.Fatal("successful deployment did not register the app")
	}

	result.Header().Set("ETag", etag)
	result.Header().Set("X-Wt-Wasm-Digest", info.Digest)
	result.Header().Set("X-Wt-Deploy-Mode", info.Mode)

	data, err := json.Marshal(s.info(a))
	if err != nil {
		t.Fatal(err)
	}

	result.WriteHeader(http.StatusOK)
	result.Body.Write(data)

	return result
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
	s.deployments = newDeploymentManager(t.TempDir())

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

	if rec.Header().Get("X-Wt-Wasm-Digest") != got.Digest {
		t.Errorf("dry run digest header = %q, want %q", rec.Header().Get("X-Wt-Wasm-Digest"), got.Digest)
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

	if rec.Header().Get("X-Wt-Deploy-Mode") != "full" {
		t.Errorf("first deploy mode = %q, want full", rec.Header().Get("X-Wt-Deploy-Mode"))
	}

	digest := digestText("wasm-one")
	if rec.Header().Get("X-Wt-Wasm-Digest") != digest {
		t.Errorf("first deploy digest = %q, want %q", rec.Header().Get("X-Wt-Wasm-Digest"), digest)
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

	if rec.Header().Get("X-Wt-Deploy-Mode") != "static" {
		t.Errorf("static deploy mode = %q, want static", rec.Header().Get("X-Wt-Deploy-Mode"))
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

	staticOnly := writeTarGz(t, map[string]string{
		"app.json":          appJSON,
		"static/index.html": "static-only",
	})

	rec = deployArchive(t, s, "demo", staticOnly, "")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Wt-Deploy-Mode") != "static" {
		t.Fatalf("component-less deploy = %d mode %q: %s", rec.Code, rec.Header().Get("X-Wt-Deploy-Mode"), rec.Body.String())
	}

	if got := appPID(s.lookup("demo")); got != pid {
		t.Errorf("component-less deploy restarted the app: pid %d, want %d", got, pid)
	}

	if static, err := os.ReadFile(filepath.Join(s.cfg.appsDir, "demo", "static", "index.html")); err != nil || string(static) != "static-only" {
		t.Errorf("component-less content = %q, %v", static, err)
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

	if rec.Header().Get("X-Wt-Deploy-Mode") != "full" {
		t.Errorf("changed deploy mode = %q, want full", rec.Header().Get("X-Wt-Deploy-Mode"))
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

func TestDeployFailureIsRecorded(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.wasmtime = filepath.Join(t.TempDir(), "missing-wasmtime")
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))
	archive := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm",
	})

	rec := deployArchive(t, s, "demo", archive, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed deploy = %d: %s", rec.Code, rec.Body.String())
	}

	history, err := s.deployments.list("demo")
	if err != nil {
		t.Fatal(err)
	}

	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1 failed attempt", len(history))
	}

	failed := history[0]
	if failed.Status != deploymentStatusFail || failed.Error == "" || failed.Digest != digestText("wasm") {
		t.Errorf("failed record = %+v, want a failed attempt with its digest and error", failed)
	}

	if _, err := s.deployments.bundle("demo", failed.ID); err == nil {
		t.Error("failed deploy became a rollback target")
	}

	if s.lookup("demo") != nil {
		t.Error("failed deploy registered the app")
	}
}

func TestCwasmOnlyBundleStarts(t *testing.T) {
	s := newLifecycleSupervisor(t)

	a := spawnTestApp(t, s, "legacy")
	if got := appPID(a); got == 0 {
		t.Fatal("cwasm-only app is not running")
	}
}

func TestDeployConditionalAndIdempotent(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.domain = "localhost"
	s.cfg.wasmtime = os.Args[0]
	s.cfg.wasmtimeTarget = "x86_64-test"
	s.cfg.cacheDir = t.TempDir()
	s.cfg.cacheMaxBytes = defaultCacheMaxBytes
	counter := filepath.Join(t.TempDir(), "compiles")
	s.cfg.wasmtimeCompiler = writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "compiled", counter, false)
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))

	t.Cleanup(func() {
		if a := s.lookup("demo"); a != nil {
			s.stop(a)
		}
	})

	archive := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm-conditional",
	})

	rec := deployArchive(t, s, "demo", archive, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first deploy = %d: %s", rec.Code, rec.Body.String())
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("deploy has no ETag")
	}

	rec = deployArchiveHeaders(t, s, "demo", archive, "", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional deploy = %d: %s", rec.Code, rec.Body.String())
	}

	history, err := s.deployments.list("demo")
	if err != nil {
		t.Fatal(err)
	}

	if len(history) != 1 {
		t.Errorf("conditional deploy added history: %d entries", len(history))
	}

	other := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm-idempotent",
	})

	rec = deployQueued(t, s, "demo", other, "", map[string]string{"Idempotency-Key": "deploy-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("idempotent first deploy = %d: %s", rec.Code, rec.Body.String())
	}

	first := rec.Body.String()
	waitDeployment(t, s, "demo", deploymentID(t, rec))

	rec = deployQueued(t, s, "demo", archive, "", map[string]string{"Idempotency-Key": "deploy-1"})
	if rec.Code != http.StatusAccepted || rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("idempotent replay = %d, header %q: %s", rec.Code, rec.Header().Get("Idempotency-Replayed"), rec.Body.String())
	}

	if rec.Body.String() != first {
		t.Error("idempotent replay returned a different body")
	}

	// A new supervisor with the same state must replay the response from disk.
	restarted := testSupervisor()
	restarted.cfg.stateDir = s.cfg.stateDir
	restarted.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))

	rec = deployQueued(t, restarted, "demo", archive, "", map[string]string{"Idempotency-Key": "deploy-1"})
	if rec.Code != http.StatusAccepted || rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay after restart = %d, header %q: %s", rec.Code, rec.Header().Get("Idempotency-Replayed"), rec.Body.String())
	}

	if rec.Body.String() != first {
		t.Error("replay after restart returned a different body")
	}

	info, err := os.Stat(filepath.Join(s.cfg.stateDir, ".deployments", "demo", deploymentReplayName))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("idempotency file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFailedDeployKeepsOldProcess(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.domain = "localhost"
	s.cfg.wasmtime = os.Args[0]
	s.cfg.wasmtimeTarget = "x86_64-test"
	s.cfg.cacheDir = t.TempDir()
	s.cfg.cacheMaxBytes = defaultCacheMaxBytes
	counter := filepath.Join(t.TempDir(), "compiles")
	s.cfg.wasmtimeCompiler = writeCompilerWithCounter(t, "wasmtime-cli 51.0.0", "compiled", counter, false)
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))

	t.Cleanup(func() {
		if a := s.lookup("demo"); a != nil {
			s.stop(a)
		}
	})

	first := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm-old",
	})
	if rec := deployArchive(t, s, "demo", first, ""); rec.Code != http.StatusOK {
		t.Fatalf("first deploy = %d: %s", rec.Code, rec.Body.String())
	}

	old := s.lookup("demo")

	oldPID := appPID(old)
	if oldPID == 0 {
		t.Fatal("first deploy did not start")
	}

	s.cfg.wasmtime = filepath.Join(t.TempDir(), "missing-wasmtime")

	second := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm-new",
	})
	if rec := deployArchive(t, s, "demo", second, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed deploy = %d: %s", rec.Code, rec.Body.String())
	}

	if next := s.lookup("demo"); next != old {
		t.Errorf("failed deploy replaced the app: %v", next)
	}

	if got := appPID(old); got != oldPID {
		t.Errorf("failed deploy changed the old pid: %d, want %d", got, oldPID)
	}
}
