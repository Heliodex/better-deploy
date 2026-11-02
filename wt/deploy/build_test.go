package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fakeWasmtime(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "wasmtime")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho wasmtime 50.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComponentInputHashTracksSDKAndNotFrontend(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	writeTestFile(t, filepath.Join(app, "go.mod"), "module demo\n\ngo 1.27.1\n\nreplace wt.packages => ../packages\n")
	writeTestFile(t, filepath.Join(app, "main.go"), "package main")
	writeTestFile(t, filepath.Join(app, "web", "src", "+page.svelte"), "<h1>one</h1>")
	writeTestFile(t, filepath.Join(root, "packages", "go.mod"), "module wt.packages\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(root, "packages", "app", "app.go"), "package app")

	first, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(app, "web", "src", "+page.svelte"), "<h1>two</h1>")
	afterFrontend, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterFrontend != first {
		t.Error("frontend change altered the component hash")
	}
	writeTestFile(t, filepath.Join(root, "packages", "app", "app.go"), "package app // changed")
	afterSDK, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterSDK == first {
		t.Error("SDK change did not alter the component hash")
	}
	writeTestFile(t, filepath.Join(app, "main.go"), "package main // changed")
	afterApp, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterApp == afterSDK {
		t.Error("app Go change did not alter the component hash")
	}
}

func TestComponentInputHashFindsParentModule(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "example")
	app := filepath.Join(module, "app")
	writeTestFile(t, filepath.Join(module, "go.mod"), "module example\n\ngo 1.27.1\n\nreplace wt.packages => ../packages\n")
	writeTestFile(t, filepath.Join(module, "go.sum"), "wt.packages v0.0.0\n")
	writeTestFile(t, filepath.Join(app, "main.go"), "package main")
	writeTestFile(t, filepath.Join(module, "other", "main.go"), "package main")
	writeTestFile(t, filepath.Join(root, "packages", "app", "app.go"), "package app")

	first, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(module, "other", "main.go"), "package main // sibling")
	afterSibling, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterSibling != first {
		t.Error("sibling app change altered the component hash")
	}
	writeTestFile(t, filepath.Join(root, "packages", "app", "app.go"), "package app // changed")
	afterSDK, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterSDK == first {
		t.Error("parent module replace target change did not alter the component hash")
	}
	writeTestFile(t, filepath.Join(module, "go.mod"), "module example\n\ngo 1.27.1\n\nreplace wt.packages => ../packages\n// changed\n")
	afterModule, err := componentInputHash(app, manifest{}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if afterModule == afterSDK {
		t.Error("parent go.mod change did not alter the component hash")
	}
}

func TestStaticInputHashTracksFrontendAndNotGo(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")
	writeTestFile(t, filepath.Join(dir, "web", "package.json"), "{}")
	writeTestFile(t, filepath.Join(dir, "web", "src", "+page.svelte"), "<h1>one</h1>")

	m := manifest{BuildStatic: "build-static"}
	first, err := staticInputHash(dir, m, m.BuildStatic)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main // changed")
	afterGo, err := staticInputHash(dir, m, m.BuildStatic)
	if err != nil {
		t.Fatal(err)
	}
	if afterGo != first {
		t.Error("Go change altered the static hash")
	}
	writeTestFile(t, filepath.Join(dir, "web", "src", "+page.svelte"), "<h1>two</h1>")
	afterFrontend, err := staticInputHash(dir, m, m.BuildStatic)
	if err != nil {
		t.Fatal(err)
	}
	if afterFrontend == first {
		t.Error("frontend change did not alter the static hash")
	}
}

func TestPrepareBuildsSplitsPhases(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")
	writeTestFile(t, filepath.Join(dir, "web", "src", "+page.svelte"), "<h1>one</h1>")

	m := manifest{
		Name:           "demo",
		BuildComponent: "printf 'component\\n' >> build.log; printf wasm > main.wasm",
		BuildStatic:    "printf 'static\\n' >> build.log; mkdir -p static; printf html > static/index.html",
	}
	cfg := &deployConfig{dir: dir, skipCompile: true, wasmtime: fakeWasmtime(t, t.TempDir()), target: "x86_64-test"}
	logPath := filepath.Join(dir, "build.log")

	first, err := prepareBuilds(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ComponentBuilt || !first.StaticBuilt || first.ComponentReused || first.StaticReused {
		t.Errorf("first prepareBuilds() = %+v, want both phases built", first)
	}
	if got := readTestFile(t, logPath); got != "component\nstatic\n" {
		t.Errorf("first build log = %q", got)
	}

	second, err := prepareBuilds(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ComponentReused || !second.StaticReused || second.ComponentBuilt || second.StaticBuilt {
		t.Errorf("second prepareBuilds() = %+v, want both phases reused", second)
	}
	if got := readTestFile(t, logPath); got != "component\nstatic\n" {
		t.Errorf("second build log = %q", got)
	}

	writeTestFile(t, filepath.Join(dir, "web", "src", "+page.svelte"), "<h1>two</h1>")
	afterFrontend, err := prepareBuilds(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if !afterFrontend.ComponentReused || !afterFrontend.StaticBuilt {
		t.Errorf("frontend prepareBuilds() = %+v, want component reused and static built", afterFrontend)
	}
	if got := readTestFile(t, logPath); got != "component\nstatic\nstatic\n" {
		t.Errorf("frontend build log = %q", got)
	}

	writeTestFile(t, filepath.Join(dir, "main.go"), "package main // changed")
	afterGo, err := prepareBuilds(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if !afterGo.ComponentBuilt || !afterGo.StaticReused {
		t.Errorf("Go prepareBuilds() = %+v, want component built and static reused", afterGo)
	}
	if got := readTestFile(t, logPath); got != "component\nstatic\nstatic\ncomponent\n" {
		t.Errorf("Go build log = %q", got)
	}
}

func TestKeepRunningComponent(t *testing.T) {
	status := buildStatus{WasmDigest: "abc"}
	if !keepRunningComponent(false, status, "abc") {
		t.Error("matching digest should keep the running component")
	}
	if keepRunningComponent(true, status, "abc") {
		t.Error("force-build should upload the component")
	}
	if keepRunningComponent(false, status, "def") {
		t.Error("different digest should upload the component")
	}
	if keepRunningComponent(false, buildStatus{}, "") {
		t.Error("missing digest should upload the component")
	}
}

func TestReadManifestValidatesBuildInputs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "app.json"), `{"name":"demo","component_inputs":["/absolute"]}`)
	if _, err := readManifest(dir); err == nil {
		t.Error("absolute component_inputs should fail")
	}
	writeTestFile(t, filepath.Join(dir, "app.json"), `{"name":"demo","static_inputs":["."]}`)
	if _, err := readManifest(dir); err == nil {
		t.Error("the app root as static_inputs should fail")
	}
	writeTestFile(t, filepath.Join(dir, "app.json"), `{"name":"demo","component_inputs":["../packages","web/src/lib/contract.ts"]}`)
	if _, err := readManifest(dir); err != nil {
		t.Errorf("relative component_inputs should pass: %v", err)
	}
}

func TestComponentInputHashIncludesExplicitInputs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")
	writeTestFile(t, filepath.Join(dir, "web", "src", "lib", "contract.ts"), "export const value = 1")
	m := manifest{ComponentInputs: []string{"web/src/lib/contract.ts"}}

	first, err := componentInputHash(dir, m, "build")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "web", "src", "lib", "contract.ts"), "export const value = 2")
	after, err := componentInputHash(dir, m, "build")
	if err != nil {
		t.Fatal(err)
	}
	if after == first {
		t.Error("explicit component input change did not alter the hash")
	}
	if strings.Contains(first, " ") {
		t.Errorf("hash looks malformed: %q", first)
	}
}
