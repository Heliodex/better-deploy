package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeClientCommand writes a bun stand-in that records its working directory and arguments, and optionally the emitted client.
func writeFakeClientCommand(t *testing.T, dir, name, record string, emit bool) {
	t.Helper()

	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"cwd=$PWD\"\n" +
		"  for arg in \"$@\"; do echo \"arg=$arg\"; done\n" +
		"  echo \"out=${WT_CLIENT_OUT:-}\"\n" +
		"} >> \"" + record + "\"\n"

	if emit {
		script += "printf '// generated\\n' > \"$WT_CLIENT_OUT\"\n"
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateRegistryOutputsResolvesRelativePaths(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	web := filepath.Join(root, "web")
	bunRecord := filepath.Join(root, "bun.log")
	bunxRecord := filepath.Join(root, "bunx.log")

	if err := os.MkdirAll(bin, privateDirMode); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(web, "biome.json"), "{}")
	writeTestFile(t, filepath.Join(web, "src", "lib", "contract.ts"), "export const value = 1")
	writeTestFile(t, filepath.Join(root, "contract", "endpoints.ts"), "export const endpoints = []")
	writeFakeClientCommand(t, bin, "bun", bunRecord, true)
	writeFakeClientCommand(t, bin, "bunx", bunxRecord, false)

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(root)

	if err := generateRegistryOutputs(".", "web"); err != nil {
		t.Fatal(err)
	}

	emitted := filepath.Join(web, "src", "lib", "api.gen.ts")
	if _, err := os.Stat(emitted); err != nil {
		t.Fatalf("client not written to %s: %v", emitted, err)
	}

	if _, err := os.Stat(filepath.Join(web, "web")); !os.IsNotExist(err) {
		t.Errorf("relative web dir was resolved twice: %v", err)
	}

	bunLog := readTestFile(t, bunRecord)
	if !strings.Contains(bunLog, "cwd="+web) {
		t.Errorf("bun cwd = %q, want %q", bunLog, web)
	}

	if !strings.Contains(bunLog, "out="+emitted) {
		t.Errorf("WT_CLIENT_OUT = %q, want %q", bunLog, emitted)
	}

	bunxLog := readTestFile(t, bunxRecord)
	if !strings.Contains(bunxLog, "arg=--config-path="+filepath.Join(web, "biome.json")) {
		t.Errorf("biome config = %q, want %q", bunxLog, filepath.Join(web, "biome.json"))
	}
}
