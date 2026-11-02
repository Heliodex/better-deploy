package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// devIgnoredDirs hold build output and dependency trees, so a rebuild does not immediately trigger another one.
var devIgnoredDirs = map[string]struct{}{
	"node_modules": {},
	".git":         {},
	".svelte-kit":  {},
	"build":        {},
	"dist":         {},
	"target":       {},
	"static":       {},
}

var devIgnoredFiles = map[string]struct{}{
	"main.wasm":     {},
	"main.cwasm":    {},
	"artifact.json": {},
}

type devFileState struct {
	modified time.Time
	size     int64
}

// devSourceState records the source files under root, ignoring build output and dependencies.
func devSourceState(root string) (map[string]devFileState, error) {
	state := map[string]devFileState{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		name := entry.Name()
		if entry.IsDir() {
			if path != root {
				if _, skip := devIgnoredDirs[name]; skip {
					return filepath.SkipDir
				}
			}

			return nil
		}

		if _, skip := devIgnoredFiles[name]; skip {
			return nil
		}

		if filepath.Ext(name) == ".gz" && len(name) > 7 && name[len(name)-7:] == ".tar.gz" {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		state[path] = devFileState{modified: info.ModTime(), size: info.Size()}

		return nil
	})

	return state, err
}

// devSourcesChanged reports whether any watched file changed size or modification time.
func devSourcesChanged(previous, next map[string]devFileState) bool {
	if len(previous) != len(next) {
		return true
	}

	for path, state := range next {
		before, ok := previous[path]
		if !ok || !before.modified.Equal(state.modified) || before.size != state.size {
			return true
		}
	}

	return false
}

// componentIgnoredDirs holds frontend and build output that Vite or the build owns, not the wasm component.
var componentIgnoredDirs = map[string]struct{}{
	".git":         {},
	".svelte-kit":  {},
	"build":        {},
	"dist":         {},
	"node_modules": {},
	"static":       {},
	"target":       {},
	"web":          {},
}

var componentIgnoredFiles = map[string]struct{}{
	"artifact.json":          {},
	"main.cwasm":             {},
	"main.wasm":              {},
	".wt-build.json":         {},
	".wt-dev.vite.config.ts": {},
}

// addDevFile records one explicit dev.watch path when it is a regular file.
func addDevFile(state map[string]devFileState, path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}

	state[path] = devFileState{modified: info.ModTime(), size: info.Size()}

	return nil
}

// componentSourceState tracks component inputs plus explicit dev.watch paths.
// The frontend directory is skipped because Vite owns those changes.
func componentSourceState(root string, extra []string) (map[string]devFileState, error) {
	state := map[string]devFileState{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		if entry.IsDir() {
			if path != root {
				if _, skip := componentIgnoredDirs[entry.Name()]; skip {
					return filepath.SkipDir
				}
			}

			return nil
		}

		if _, skip := componentIgnoredFiles[entry.Name()]; skip {
			return nil
		}

		if strings.HasSuffix(entry.Name(), ".gen.go") {
			return nil
		}

		return addDevFile(state, path)
	})
	if err != nil {
		return nil, err
	}

	for _, value := range extra {
		path := filepath.Join(root, value)

		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		if !info.IsDir() {
			if err := addDevFile(state, path); err != nil {
				return nil, err
			}

			continue
		}

		if err := filepath.WalkDir(path, func(child string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}

			return addDevFile(state, child)
		}); err != nil {
			return nil, err
		}
	}

	return state, nil
}

// watchComponent rebuilds only the wasm component and leaves the Vite dev server running.
func watchComponent(cfg *deployConfig, extra []string, done <-chan struct{}) {
	previous, err := componentSourceState(cfg.dir, extra)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev: component watch:", err)
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		next, err := componentSourceState(cfg.dir, extra)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: component watch:", err)
			continue
		}

		if !devSourcesChanged(previous, next) {
			previous = next
			continue
		}

		time.Sleep(400 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "dev: component change, rebuilding")

		m, data, status, err := buildBundle(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: component rebuild failed:", err)

			previous, _ = componentSourceState(cfg.dir, extra)

			continue
		}

		printBuildStatus(status)

		if err := upload(cfg, m, data); err != nil {
			fmt.Fprintln(os.Stderr, "dev: component redeploy failed:", err)

			previous, _ = componentSourceState(cfg.dir, extra)

			continue
		}

		previous, _ = componentSourceState(cfg.dir, extra)
		fmt.Printf("dev: redeployed %s at %s/\n", m.Name, cfg.host)
	}
}

// watchDev rebuilds and redeploys the app when a source file changes, until done closes.
func watchDev(cfg *deployConfig, done <-chan struct{}) {
	previous, err := devSourceState(cfg.dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev: watch:", err)
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		next, err := devSourceState(cfg.dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: watch:", err)
			continue
		}

		if !devSourcesChanged(previous, next) {
			previous = next
			continue
		}
		// Let a build tool that is still writing its source files settle before rebuilding.
		time.Sleep(400 * time.Millisecond)

		previous, _ = devSourceState(cfg.dir)

		fmt.Fprintln(os.Stderr, "dev: change detected, rebuilding")

		m, data, status, err := buildBundle(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: rebuild failed:", err)
			continue
		}

		printBuildStatus(status)

		if err := upload(cfg, m, data); err != nil {
			fmt.Fprintln(os.Stderr, "dev: redeploy failed:", err)
			continue
		}

		fmt.Printf("dev: redeployed %s at %s/\n", m.Name, cfg.host)
	}
}
