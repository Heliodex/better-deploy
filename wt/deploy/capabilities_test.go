package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func capabilityTestServer(t *testing.T, caps supervisorCapabilities, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capabilities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			data, err := json.Marshal(caps)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(data)
		}
	}))
}

func TestFetchCapabilities(t *testing.T) {
	want := supervisorCapabilities{
		Version:        "1.2.3",
		Commit:         "abcdef0123456789",
		GoVersion:      "go1.27.1",
		Wasmtime:       "wasmtime 50.0.0",
		WasmtimeTarget: "x86_64-test",
		Compiler:       true,
	}
	server := capabilityTestServer(t, want, http.StatusOK)
	defer server.Close()

	got, err := fetchCapabilities(clientOptions{host: server.URL, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || got.Commit != want.Commit || got.WasmtimeTarget != want.WasmtimeTarget {
		t.Errorf("capabilities = %+v", got)
	}
	if got.displayVersion() != "1.2.3+abcdef012345" {
		t.Errorf("displayVersion = %q", got.displayVersion())
	}
	if got.summary() != "supervisor: 1.2.3+abcdef012345; wasmtime 50.0.0 (x86_64-test)" {
		t.Errorf("summary = %q", got.summary())
	}
}

func TestFetchCapabilitiesOldSupervisor(t *testing.T) {
	server := capabilityTestServer(t, supervisorCapabilities{}, http.StatusNotFound)
	defer server.Close()

	_, err := fetchCapabilities(clientOptions{host: server.URL, timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want a missing-capabilities status", err)
	}
}

func TestPreflightDeployUsesServerTarget(t *testing.T) {
	bin := fakeWasmtime(t, t.TempDir())
	server := capabilityTestServer(t, supervisorCapabilities{
		Version:        "1.2.3",
		Wasmtime:       "wasmtime 50.0.0",
		WasmtimeTarget: "x86_64-test",
		Compiler:       true,
	}, http.StatusOK)
	defer server.Close()

	cfg := &deployConfig{host: server.URL, wasmtime: bin, target: "x86_64-musl"}
	pre, err := preflightDeploy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.target != "x86_64-test" || !pre.TargetFromServer {
		t.Errorf("target = %q, from server %v", cfg.target, pre.TargetFromServer)
	}
	if !pre.RuntimeKnown || !pre.RuntimeMatch {
		t.Errorf("preflight = %+v, want a matching runtime", pre)
	}
}

func TestPreflightDeployKeepsExplicitTarget(t *testing.T) {
	bin := fakeWasmtime(t, t.TempDir())
	server := capabilityTestServer(t, supervisorCapabilities{
		Version:        "1.2.3",
		Wasmtime:       "wasmtime 50.0.0",
		WasmtimeTarget: "x86_64-test",
		Compiler:       true,
	}, http.StatusOK)
	defer server.Close()

	cfg := &deployConfig{host: server.URL, wasmtime: bin, target: "x86_64-custom", targetConfigured: true}
	pre, err := preflightDeploy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.target != "x86_64-custom" || pre.TargetFromServer {
		t.Errorf("target = %q, from server %v, want the explicit target", cfg.target, pre.TargetFromServer)
	}
	if pre.RuntimeMatch {
		t.Error("mismatched explicit target should not match")
	}
}

func TestPreflightDeployRejectsOldSupervisor(t *testing.T) {
	server := capabilityTestServer(t, supervisorCapabilities{}, http.StatusNotFound)
	defer server.Close()

	cfg := &deployConfig{host: server.URL, target: "x86_64-test"}
	if _, err := preflightDeploy(cfg); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want a missing-capabilities status", err)
	}
}

func TestFakeWasmtimeTarget(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wasmtime")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho wasmtime 50.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := wasmtimeVersion(bin)
	if err != nil || version != "wasmtime 50.0.0" {
		t.Errorf("wasmtimeVersion = %q, %v", version, err)
	}
}
