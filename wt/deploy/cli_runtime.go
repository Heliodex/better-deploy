package main

import (
	"fmt"
	"strings"
	"time"
)

// runRuntime reports the local and supervisor wasmtime releases and target compatibility.
func runRuntime(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	flags := newFlagSet("runtime")

	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME"), "wasmtime"), "local wasmtime binary")
	if err := flags.Parse(rest); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	caps, err := fetchCapabilities(clientOptions{host: cfg.host, token: cfg.token, timeout: 10 * time.Second})
	if err != nil {
		return err
	}

	local, localErr := wasmtimeVersion(*wasmtime)

	if cfg.jsonOut {
		report := map[string]any{
			"supervisor": caps.Wasmtime,
			"target":     caps.WasmtimeTarget,
			"compiler":   caps.Compiler,
			"local":      local,
		}
		if localErr != nil {
			report["error"] = localErr.Error()
		} else {
			report["releaseMatch"] = runtimeRelease(local) == runtimeRelease(caps.Wasmtime)
		}

		printRawOrJSONJSON(report)

		if localErr != nil {
			return localErr
		}

		return nil
	}

	fmt.Printf("supervisor: %s", displayRuntime(caps.Wasmtime, caps.WasmtimeTarget))
	fmt.Println()

	if localErr != nil {
		fmt.Printf("local: unavailable (%v)\n", localErr)
		return localErr
	}

	fmt.Printf("local: %s\n", local)

	switch {
	case caps.Wasmtime == "":
		fmt.Println("release: supervisor did not report a wasmtime version")
	case runtimeRelease(local) == runtimeRelease(caps.Wasmtime):
		fmt.Println("release: match")
	default:
		fmt.Printf("release: mismatch (%s vs %s)\n", runtimeRelease(local), runtimeRelease(caps.Wasmtime))
	}

	if caps.WasmtimeTarget == "" {
		fmt.Println("target: supervisor did not report a target")
	} else {
		fmt.Printf("target: %s\n", caps.WasmtimeTarget)
	}

	fmt.Printf("compiler: %v\n", caps.Compiler)

	return nil
}

// displayRuntime renders a runtime and target for one line.
func displayRuntime(version, target string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "wasmtime unknown"
	}

	if strings.TrimSpace(target) != "" {
		version += " (" + strings.TrimSpace(target) + ")"
	}

	return version
}
