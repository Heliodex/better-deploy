package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postDeployment(t *testing.T, s *supervisor, name, archive string) *httptest.ResponseRecorder {
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

	req := httptest.NewRequest(http.MethodPost, "/apps/"+name+"/deployments", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	s.handleApp(rec, req)

	return rec
}

func getDeployment(t *testing.T, s *supervisor, name, id string) (*httptest.ResponseRecorder, deploymentInfo) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps/"+name+"/deployments/"+id, nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)

	var info deploymentInfo
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
	}

	return rec, info
}

func TestAsyncDeploymentResource(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.domain = "localhost"
	s.cfg.wasmtimeTarget = "x86_64-test"
	s.cfg.cacheDir = t.TempDir()
	s.cfg.cacheMaxBytes = defaultCacheMaxBytes
	s.cfg.wasmtimeCompiler = writeCompiler(t, "wasmtime-cli 51.0.0", "cwasm", false)
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))

	t.Cleanup(func() {
		if a := s.lookup("demo"); a != nil {
			s.stop(a)
		}
	})

	archive := writeTarGz(t, map[string]string{
		"app.json":  `{"name":"demo","host":"demo.localhost"}`,
		"main.wasm": "wasm",
	})

	rec := postDeployment(t, s, "demo", archive)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create deployment = %d: %s", rec.Code, rec.Body.String())
	}

	var created deploymentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created.ID == "" || created.Status != deploymentStatusQueued {
		t.Errorf("created deployment = %+v", created)
	}

	location := rec.Header().Get("Location")
	if location != "/apps/demo/deployments/"+created.ID {
		t.Errorf("Location = %q", location)
	}

	var info deploymentInfo

	deadline := time.Now().Add(10 * time.Second)

	for {
		_, info = getDeployment(t, s, "demo", created.ID)
		if info.Status == deploymentStatusOK || info.Status == deploymentStatusFail || info.Status == deploymentStatusCanceled {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("deployment did not finish: %+v", info)
		}

		time.Sleep(20 * time.Millisecond)
	}

	if info.Status != deploymentStatusOK {
		t.Fatalf("deployment status = %q: %s", info.Status, info.Error)
	}

	if info.Mode != "full" {
		t.Errorf("deployment mode = %q, want full", info.Mode)
	}

	if s.lookup("demo") == nil {
		t.Error("successful deployment did not register the app")
	}

	logRec := httptest.NewRecorder()
	logReq := httptest.NewRequest(http.MethodGet, "/apps/demo/deployments/"+created.ID+"/logs", nil)
	logReq.Header.Set("Authorization", "Bearer secret")
	s.handleApp(logRec, logReq)

	if logRec.Code != http.StatusOK {
		t.Fatalf("deployment logs = %d: %s", logRec.Code, logRec.Body.String())
	}

	if !strings.Contains(logRec.Body.String(), "succeeded") {
		t.Errorf("deployment log = %q", logRec.Body.String())
	}

	streamRec := httptest.NewRecorder()
	streamReq := httptest.NewRequest(http.MethodGet, "/apps/demo/deployments/"+created.ID+"/logs/stream", nil)
	streamReq.Header.Set("Authorization", "Bearer secret")
	s.handleApp(streamRec, streamReq)

	if streamRec.Code != http.StatusOK {
		t.Fatalf("deployment stream = %d: %s", streamRec.Code, streamRec.Body.String())
	}

	if !strings.Contains(streamRec.Body.String(), "event: done") {
		t.Errorf("stream did not finish: %q", streamRec.Body.String())
	}

	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/apps/demo/deployments/"+created.ID+"/cancel", nil)
	cancelReq.Header.Set("Authorization", "Bearer secret")
	s.handleApp(cancelRec, cancelReq)

	if cancelRec.Code != http.StatusConflict {
		t.Errorf("cancel terminal deployment = %d, want 409", cancelRec.Code)
	}
}

func TestAsyncDeploymentRejectsBadBundle(t *testing.T) {
	s := newLifecycleSupervisor(t)
	s.cfg.domain = "localhost"
	s.deployments = newDeploymentManager(filepath.Join(s.cfg.stateDir, ".deployments"))
	archive := writeTarGz(t, map[string]string{"app.json": `{"name":"demo"}`})

	rec := postDeployment(t, s, "demo", archive)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("deployment without a component = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
