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

func TestDeployDryRunValidatesWithoutInstalling(t *testing.T) {
	archive := writeTarGz(t, map[string]string{
		"app.json":   `{"name":"demo","host":"demo.localhost"}`,
		"main.cwasm": "cwasm",
	})
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

	s := testSupervisor()
	s.cfg.appsDir = t.TempDir()
	s.cfg.domain = "localhost"
	req := httptest.NewRequest(http.MethodPost, "/apps/demo?dry_run=1", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleApp(rec, req)
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
	if s.lookup("demo") != nil {
		t.Error("dry run registered the app")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.appsDir, "demo")); !os.IsNotExist(err) {
		t.Errorf("dry run installed an app directory: %v", err)
	}
}
