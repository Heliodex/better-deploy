package main

import (
	"encoding/json/v2"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExampleLayout(t *testing.T, root string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, "example"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "example", "go.mod"), []byte("module example.test\n\ngo 1.27.1\n"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "packages"), privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "packages", "go.mod"), []byte("module github.com/heliodex/better-deploy/wt/packages\n"), privateFileMode); err != nil {
		t.Fatal(err)
	}
}

// TestNewMatchesTemplate fails if a template file is missing from the embed list in new.go, or if node_modules or bun.lock leak into the embedded set.
func TestNewMatchesTemplate(t *testing.T) {
	embedded := map[string]bool{}

	err := fs.WalkDir(templateFS, "testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		rel := strings.TrimPrefix(path, "testdata/template/")
		embedded[rel] = true

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	onDisk := map[string]bool{}

	err = filepath.WalkDir("testdata/template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(filepath.Join("testdata", "template"), path)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if rel == "web" || strings.HasPrefix(rel, "web/") {
			name := strings.TrimPrefix(rel, "web/")
			if name == "node_modules" || strings.HasPrefix(name, "node_modules/") || name == "bun.lock" {
				if entry.IsDir() {
					return filepath.SkipDir
				}

				return nil
			}
			// Generated output that bun install and svelte-kit sync leave behind.
			if name == ".svelte-kit" || strings.HasPrefix(name, ".svelte-kit/") || name == "build" || strings.HasPrefix(name, "build/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}

				return nil
			}
		}

		if entry.IsDir() {
			return nil
		}

		onDisk[rel] = true

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for rel := range onDisk {
		if !embedded[rel] {
			t.Errorf("template file %s is not embedded; add it to the go:embed list in new.go", rel)
		}
	}

	for rel := range embedded {
		if !onDisk[rel] {
			t.Errorf("embedded file %s is missing on disk", rel)
		}
	}
}

// TestTemplateHandlerUsesServeHTTP guards the wasihttp.Handle call against the template passing an http.Handler where a handler function is required.
func TestTemplateHandlerUsesServeHTTP(t *testing.T) {
	data, err := templateFS.ReadFile("testdata/template/wasm.go")
	if err != nil {
		t.Fatal(err)
	}

	src := strings.ReplaceAll(string(data), newContractToken, "example.test/contract")

	file, err := parser.ParseFile(token.NewFileSet(), "wasm.go", src, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}

	found := false

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handle" {
			return true
		}

		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "wasihttp" {
			return true
		}

		found = true

		if len(call.Args) != 1 {
			t.Errorf("wasihttp.Handle has %d arguments, want 1", len(call.Args))
			return true
		}

		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok || arg.Sel.Name != "ServeHTTP" {
			t.Errorf("wasihttp.Handle argument must be a .ServeHTTP method value")
		}

		return true
	})

	if !found {
		t.Error("template has no wasihttp.Handle call")
	}
}

func TestPrepareNewDirAllowsNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("keep"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	nonEmpty, err := prepareNewDir(dir, dir)
	if err != nil || !nonEmpty {
		t.Errorf("prepareNewDir(non-empty) = %v, %v", nonEmpty, err)
	}

	missing := filepath.Join(t.TempDir(), "fresh")

	nonEmpty, err = prepareNewDir(missing, missing)
	if err != nil || nonEmpty {
		t.Errorf("prepareNewDir(missing) = %v, %v", nonEmpty, err)
	}

	if info, err := os.Stat(missing); err != nil || !info.IsDir() {
		t.Errorf("prepareNewDir did not create the directory: %v", err)
	}
}

func TestRunNew(t *testing.T) {
	root := t.TempDir()
	writeExampleLayout(t, root)
	dir := filepath.Join(root, "example", "myapp")

	if err := runNew([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"app.json",
		"biome.json",
		".oxlintrc.json",
		"main.go",
		"main_test.go",
		"wasm.go",
		".gitignore",
		"web/package.json",
		"web/src/lib/api.ts",
		"web/src/lib/contract.ts",
		"web/src/routes/+page.svelte",
		"web/src/routes/+layout.svelte",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	for _, name := range []string{"contract", "web/scripts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s should be generated by the CLI, not scaffolded", name)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}

	var manifest struct {
		Name           string `json:"name"`
		Host           string `json:"host"`
		BuildComponent string `json:"build_component"`
		BuildStatic    string `json:"build_static"`
		Contract       string `json:"contract"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	if manifest.Name != "myapp" || manifest.Host != "myapp.localhost" {
		t.Errorf("manifest = %+v", manifest)
	}

	if manifest.BuildComponent == "" || manifest.BuildStatic == "" {
		t.Errorf("manifest is missing split build commands: %+v", manifest)
	}

	if manifest.Contract != "valibot" {
		t.Errorf("manifest contract = %q, want valibot", manifest.Contract)
	}

	main, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(main), `"example.test/myapp/contract"`) {
		t.Errorf("main.go does not import the app contract:\n%s", main)
	}

	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", main, parser.AllErrors); err != nil {
		t.Errorf("main.go does not parse: %v", err)
	}

	contractSource, err := os.ReadFile(filepath.Join(dir, "web", "src", "lib", "contract.ts"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"requestSchemas", "responseSchemas", `title("NotesResponse")`, `title("ErrorResponse")`} {
		if !strings.Contains(string(contractSource), want) {
			t.Errorf("contract.ts is missing %q:\n%s", want, contractSource)
		}
	}

	if !strings.Contains(string(main), "contract.NotesResponse") {
		t.Errorf("main.go does not return the generated response type:\n%s", main)
	}

	sentinel := filepath.Join(dir, "KEEP.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := runNew([]string{"--dir", dir}); err != nil {
		t.Errorf("scaffolding into a non-empty directory: %v", err)
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("scaffolding removed an unrelated file: %v", err)
	}

	if err := runNew([]string{"--dir", filepath.Join(root, "example", "Bad_Name")}); err == nil {
		t.Error("expected error for an invalid app name")
	}

	outside := filepath.Join(t.TempDir(), "elsewhere")
	if _, _, err := scaffoldNew(&newConfig{dir: outside, contract: "valibot"}); err != nil {
		t.Fatalf("standalone scaffold: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outside, "sdk")); !os.IsNotExist(err) {
		t.Errorf("standalone scaffold still copies an sdk directory: %v", err)
	}

	standaloneMain, err := os.ReadFile(filepath.Join(outside, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(standaloneMain), `"elsewhere/contract"`) {
		t.Errorf("standalone main.go does not use its own module contract:\n%s", standaloneMain)
	}

	standaloneApp, err := os.ReadFile(filepath.Join(outside, "app.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(standaloneApp), "go list -m -f '{{.Dir}}' "+sdkModulePath) || strings.Contains(string(standaloneApp), "generate.sh") {
		t.Errorf("standalone app.json does not use the shared SDK module:\n%s", standaloneApp)
	}

	module, err := os.ReadFile(filepath.Join(outside, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(module), "require "+sdkModulePath+" "+sdkModuleVersion) || strings.Contains(string(module), "replace ") || !strings.Contains(string(module), "tool (") {
		t.Errorf("standalone go.mod is incomplete:\n%s", module)
	}
}

func TestScaffoldNewLocalSDKReplace(t *testing.T) {
	root := t.TempDir()

	sdk := filepath.Join(root, "packages")
	if err := os.MkdirAll(sdk, privateDirMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(sdk, "go.mod"), []byte("module "+sdkModulePath+"\n"), privateFileMode); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "sdkapp")
	if _, _, err := scaffoldNew(&newConfig{dir: dir, sdk: sdk, contract: "valibot"}); err != nil {
		t.Fatal(err)
	}

	module, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	wantReplace := "replace " + sdkModulePath + " => " + filepath.ToSlash(sdk)

	if !strings.Contains(string(module), wantReplace) {
		t.Errorf("go.mod has no local SDK replace %q:\n%s", wantReplace, module)
	}

	if err := validateLocalSDK(t.TempDir()); err == nil {
		t.Error("validateLocalSDK accepted a directory without the SDK go.mod")
	}
}

func TestRunNewContractless(t *testing.T) {
	root := t.TempDir()
	writeExampleLayout(t, root)
	dir := filepath.Join(root, "example", "plainnotes")

	if _, _, err := scaffoldNew(&newConfig{dir: dir, contract: "none"}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"main.go", "main_test.go", "web/src/lib/contract.ts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	for _, name := range []string{"contract", "web/scripts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s should not exist in a contractless scaffold", name)
		}
	}

	main, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(main), `plainnotes/contract`) || !strings.Contains(string(main), "type note struct") {
		t.Errorf("main.go still uses the generated contract:\n%s", main)
	}

	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", main, parser.AllErrors); err != nil {
		t.Errorf("main.go does not parse: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(manifest), `"contract"`) || strings.Contains(string(manifest), `web/src/lib/contract.ts`) {
		t.Errorf("app.json still references the contract pipeline:\n%s", manifest)
	}

	if err := runNew([]string{"--dir", filepath.Join(root, "example", "badcontract"), "--contract", "prisma"}); err == nil {
		t.Error("unknown contract mode was accepted")
	}
}

func TestRunContractMissingSchemas(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"demo"}`), privateFileMode); err != nil {
		t.Fatal(err)
	}

	if err := runContract([]string{"--dir", dir}); err == nil || !strings.Contains(err.Error(), "no web/src/lib/contract.ts") {
		t.Errorf("error = %v", err)
	}
}
