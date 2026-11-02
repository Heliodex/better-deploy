package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"runtime/debug"
)

// version is overridden with -ldflags "-X main.version=..." in a release build.
var version = "dev"

// capabilitiesResponse describes the supervisor and its runtime.
type capabilitiesResponse struct {
	Version        string `json:"version"`
	Commit         string `json:"commit,omitempty"`
	GoVersion      string `json:"goVersion"`
	Wasmtime       string `json:"wasmtime,omitempty"`
	WasmtimeTarget string `json:"wasmtimeTarget,omitempty"`
	Compiler       bool   `json:"compiler"`
	Epoch          bool   `json:"epoch"`
}

// supervisorBuildInfo reads the main module version and VCS settings from the Go build.
func supervisorBuildInfo() (string, string) {
	name := version
	commit := ""

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return name, commit
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		name = info.Main.Version
	}

	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			commit = setting.Value
		}
	}

	return name, commit
}

// canCompile reports whether the configured compiler has a compile subcommand. The runtime-only image does not.
func (s *supervisor) canCompile() bool {
	s.compilerOnce.Do(func() {
		if s.cfg.wasmtimeCompiler == "" {
			return
		}

		cmd := exec.CommandContext(context.Background(), s.cfg.wasmtimeCompiler, "compile", "--help")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		s.compilerOK = cmd.Run() == nil
	})

	return s.compilerOK
}

// handleCapabilities serves the version and runtime document clients use to preflight a deploy.
func (s *supervisor) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	if _, ok := s.requireIdentity(w, r); !ok {
		return
	}

	name, commit := supervisorBuildInfo()
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Version:        name,
		Commit:         commit,
		GoVersion:      runtime.Version(),
		Wasmtime:       s.wasmtimeVersion(),
		WasmtimeTarget: s.cfg.wasmtimeTarget,
		Compiler:       s.canCompile(),
		Epoch:          true,
	})
}
