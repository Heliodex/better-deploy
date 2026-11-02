package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// supervisorCapabilities is the runtime document served by a supervisor.
type supervisorCapabilities struct {
	Version        string `json:"version"`
	Commit         string `json:"commit,omitempty"`
	GoVersion      string `json:"goVersion,omitempty"`
	Wasmtime       string `json:"wasmtime,omitempty"`
	WasmtimeTarget string `json:"wasmtimeTarget,omitempty"`
	Compiler       bool   `json:"compiler"`
	Epoch          bool   `json:"epoch"`
}

// deployPreflight is the result of asking the supervisor what it can do before a build starts.
type deployPreflight struct {
	Checked          bool
	Capabilities     supervisorCapabilities
	RuntimeKnown     bool
	RuntimeMatch     bool
	TargetFromServer bool
}

// displayVersion renders the supervisor version with a short commit when one was recorded.
func (c supervisorCapabilities) displayVersion() string {
	name := strings.TrimSpace(c.Version)
	if name == "" {
		name = "unknown"
	}

	commit := strings.TrimSpace(c.Commit)
	if len(commit) > 12 {
		commit = commit[:12]
	}

	if commit == "" {
		return name
	}

	return name + "+" + commit
}

// summary renders one line for wt apps showing the supervisor build and runtime.
func (c supervisorCapabilities) summary() string {
	runtime := strings.TrimSpace(c.Wasmtime)
	if runtime == "" {
		runtime = "wasmtime unknown"
	}

	if target := strings.TrimSpace(c.WasmtimeTarget); target != "" {
		runtime += " (" + target + ")"
	}

	return "supervisor: " + c.displayVersion() + "; " + runtime
}

// fetchCapabilities asks the supervisor for its version and runtime.
func fetchCapabilities(client clientOptions) (supervisorCapabilities, error) {
	status, _, body, err := apiDo(client, http.MethodGet, "/capabilities", nil, "")
	if err != nil {
		return supervisorCapabilities{}, fmt.Errorf("query supervisor capabilities: %w", err)
	}

	if err := checkAPI(status, body); err != nil {
		return supervisorCapabilities{}, fmt.Errorf("query supervisor capabilities: %w", err)
	}

	var caps supervisorCapabilities
	if err := json.Unmarshal(body, &caps); err != nil {
		return supervisorCapabilities{}, fmt.Errorf("decode supervisor capabilities: %w", err)
	}

	return caps, nil
}

// preflightDeploy negotiates the runtime target before a build and reports whether the local cwasm will match.
func preflightDeploy(cfg *deployConfig) (deployPreflight, error) {
	caps, err := fetchCapabilities(clientOptions{host: cfg.host, token: cfg.token, timeout: 10 * time.Second})
	if err != nil {
		return deployPreflight{}, fmt.Errorf("preflight %s: %w", cfg.host, err)
	}

	pre := deployPreflight{Checked: true, Capabilities: caps}
	if caps.WasmtimeTarget != "" && !cfg.targetConfigured {
		cfg.target = caps.WasmtimeTarget
		pre.TargetFromServer = true
	}

	localVersion, err := wasmtimeVersion(cfg.wasmtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: local wasmtime unavailable: %v\n", err)
		return pre, nil
	}

	targetMatch := caps.WasmtimeTarget == "" || strings.TrimSpace(cfg.target) == strings.TrimSpace(caps.WasmtimeTarget)
	versionMatch := caps.Wasmtime == "" || strings.TrimSpace(localVersion) == strings.TrimSpace(caps.Wasmtime)
	pre.RuntimeKnown = caps.Wasmtime != "" && caps.WasmtimeTarget != ""

	pre.RuntimeMatch = pre.RuntimeKnown && targetMatch && versionMatch
	if !targetMatch {
		fmt.Fprintf(os.Stderr, "warning: target %q differs from supervisor target %q; the supervisor will recompile from main.wasm\n", cfg.target, caps.WasmtimeTarget)
	}

	if !versionMatch {
		fmt.Fprintf(os.Stderr, "warning: local wasmtime %q differs from supervisor %q; the supervisor will recompile from main.wasm\n", localVersion, caps.Wasmtime)
	}

	if !pre.RuntimeMatch && !caps.Compiler {
		fmt.Fprintln(os.Stderr, "warning: supervisor has no compiler and the uploaded cwasm will not match")
	}

	return pre, nil
}

// printRuntimePlan tells the operator whether the uploaded cwasm can be used or the supervisor will compile.
func printRuntimePlan(pre deployPreflight, hasCwasm bool) {
	if !pre.Checked {
		return
	}

	if !hasCwasm {
		fmt.Fprintln(os.Stderr, "casm: none uploaded; supervisor will compile main.wasm")
		return
	}

	if pre.RuntimeMatch {
		fmt.Fprintln(os.Stderr, "casm: matches supervisor runtime and target")
		return
	}

	if pre.Capabilities.Compiler {
		fmt.Fprintln(os.Stderr, "casm: supervisor will recompile main.wasm")
		return
	}

	fmt.Fprintln(os.Stderr, "casm: mismatch and supervisor has no compiler; deploy may fail")
}
