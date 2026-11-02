package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleCapabilities(t *testing.T) {
	s := testSupervisor()
	bin := filepath.Join(t.TempDir(), "wasmtime")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho wasmtime 50.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.wasmtime = bin
	s.cfg.wasmtimeCompiler = bin
	s.cfg.wasmtimeTarget = "x86_64-test"

	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	s.handleCapabilities(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated capabilities = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleCapabilities(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities = %d: %s", rec.Code, rec.Body.String())
	}
	var caps capabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.CapabilitiesVersion != capabilitiesVersion || caps.DeployProtocol != deployProtocolVersion {
		t.Errorf("versions = %d/%d, want %d/%d", caps.CapabilitiesVersion, caps.DeployProtocol, capabilitiesVersion, deployProtocolVersion)
	}
	if caps.Version == "" || caps.GoVersion == "" {
		t.Errorf("build fields are empty: %+v", caps)
	}
	if caps.Wasmtime != "wasmtime 50.0.0" || caps.WasmtimeTarget != "x86_64-test" {
		t.Errorf("runtime = %q target %q", caps.Wasmtime, caps.WasmtimeTarget)
	}
	if !caps.Compiler || !caps.Epoch {
		t.Errorf("compiler=%v epoch=%v, want both true", caps.Compiler, caps.Epoch)
	}
	for _, feature := range []string{"deployments", "jobs", "blobs", "identity", "events", "backups", "database"} {
		if !caps.Features[feature] {
			t.Errorf("feature %q is not advertised", feature)
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleCapabilities(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST capabilities = %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestCapabilitiesWithoutCompiler(t *testing.T) {
	s := testSupervisor()
	s.cfg.token = ""
	s.cfg.wasmtimeCompiler = filepath.Join(t.TempDir(), "missing-wasmtime")

	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	s.handleCapabilities(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities = %d: %s", rec.Code, rec.Body.String())
	}
	var caps capabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Compiler {
		t.Error("missing compiler was advertised as available")
	}
	if !caps.Features["deployments"] {
		t.Error("deployments should still be advertised without a compiler")
	}
}
