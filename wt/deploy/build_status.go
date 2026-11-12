package main

import (
	"fmt"
	"os"
)

// buildStatus reports which phases ran, which were reused and the resulting component digest.
type buildStatus struct {
	ComponentBuilt, ComponentReused, ComponentRecompiled, StaticBuilt, StaticReused, StaticSkipped, BuildSkipped bool
	ComponentHash, StaticHash                                                                                    string
	ComponentFiles, StaticFiles                                                                                  int
	WasmDigest                                                                                                   string
}

// buildDetail renders the auditable cache evidence for one phase: the short input hash and the file count behind it.
func buildDetail(hash string, files int) string {
	if hash == "" {
		return ""
	}

	short := hash
	if len(short) > shortHashLength {
		short = short[:shortHashLength]
	}

	unit := "files"
	if files == 1 {
		unit = "file"
	}

	return fmt.Sprintf(" (%s, %d %s)", short, files, unit)
}

// printBuildStatus writes the phase summary to stderr, keeping stdout free for JSON and bundle output.
func printBuildStatus(status buildStatus) {
	if status.BuildSkipped {
		fmt.Fprintln(os.Stderr, "build: skipped")
		return
	}

	switch {
	case status.ComponentBuilt:
		fmt.Fprintln(os.Stderr, "component: built"+buildDetail(status.ComponentHash, status.ComponentFiles))
	case status.ComponentRecompiled:
		fmt.Fprintln(os.Stderr, "component: recompiled"+buildDetail(status.ComponentHash, status.ComponentFiles))
	case status.ComponentReused:
		fmt.Fprintln(os.Stderr, "component: reused"+buildDetail(status.ComponentHash, status.ComponentFiles))
	}

	switch {
	case status.StaticBuilt:
		fmt.Fprintln(os.Stderr, "static: built"+buildDetail(status.StaticHash, status.StaticFiles))
	case status.StaticReused:
		fmt.Fprintln(os.Stderr, "static: reused"+buildDetail(status.StaticHash, status.StaticFiles))
	case status.StaticSkipped:
		fmt.Fprintln(os.Stderr, "static: skipped")
	default:
		fmt.Fprintln(os.Stderr, "static: no build command")
	}
}

// keepRunningComponent reports whether a deploy can skip the wasm payload because the supervisor already runs this digest.
func keepRunningComponent(forceBuild bool, status buildStatus, serverDigest string) bool {
	return !forceBuild && status.WasmDigest != "" && status.WasmDigest == serverDigest
}
