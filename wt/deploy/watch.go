package main

import (
	"fmt"
	"os"
	"path/filepath"
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
		m, data, err := buildBundle(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: rebuild failed:", err)
			continue
		}
		if err := upload(cfg, m, data); err != nil {
			fmt.Fprintln(os.Stderr, "dev: redeploy failed:", err)
			continue
		}
		fmt.Printf("dev: redeployed %s at %s/\n", m.Name, cfg.host)
	}
}
