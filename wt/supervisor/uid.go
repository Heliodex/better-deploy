// Per-app UID/GID allocation for untrusted tenants.

package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// uidState is the persisted name-to-uid mapping. UIDs are never reused, so an app keeps its identity across restarts and deletions.
type uidState struct {
	Apps map[string]int `json:"apps"`
}

// uidAllocator hands out stable dedicated UIDs from a base, persisting them under WT_STATE_DIR/.uids.json.
type uidAllocator struct {
	mu   sync.Mutex
	path string
	base int
	apps map[string]int
	used map[int]string
}

func newUIDAllocator(path string, base int) (*uidAllocator, error) {
	if base <= 0 || base > 1<<31-1 {
		return nil, fmt.Errorf("WT_APP_UID_BASE must be a positive 32-bit uid, got %d", base)
	}

	a := &uidAllocator{
		path: path,
		base: base,
		apps: map[string]int{},
		used: map[int]string{},
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var state uidState
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		for name, uid := range state.Apps {
			if validName(name) && uid >= base {
				a.apps[name] = uid
			}
		}
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Resolve collisions deterministically: sorted names keep the first claim and reassign the rest.
	names := make([]string, 0, len(a.apps))
	for name := range a.apps {
		names = append(names, name)
	}

	sort.Strings(names)

	changed := false

	for _, name := range names {
		uid := a.apps[name]
		if other, ok := a.used[uid]; ok && other != name {
			delete(a.apps, name)

			changed = true

			continue
		}

		a.used[uid] = name
	}

	for _, name := range names {
		if _, ok := a.apps[name]; ok {
			continue
		}

		uid, err := a.nextLocked()
		if err != nil {
			return nil, err
		}

		a.apps[name] = uid
		a.used[uid] = name
		changed = true
	}

	if changed {
		if err := a.saveLocked(); err != nil {
			return nil, err
		}
	}

	return a, nil
}

// nextLocked finds the lowest free uid at or above the base. The caller holds a.mu.
func (a *uidAllocator) nextLocked() (int, error) {
	for uid := a.base; uid < a.base+1_000_000; uid++ {
		if _, used := a.used[uid]; !used {
			return uid, nil
		}
	}

	return 0, fmt.Errorf("no free app uid at or above %d", a.base)
}

func (a *uidAllocator) saveLocked() error {
	data, err := json.Marshal(uidState{Apps: a.apps})
	if err != nil {
		return err
	}

	if err := writeFileAtomic(a.path, append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", a.path, err)
	}

	if err := os.Chmod(a.path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", a.path, err)
	}

	return nil
}

// Allocate returns the app's stable uid, assigning and persisting a new one on first use.
func (a *uidAllocator) Allocate(name string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if uid, ok := a.apps[name]; ok {
		return uid, nil
	}

	uid, err := a.nextLocked()
	if err != nil {
		return 0, err
	}

	a.apps[name] = uid

	a.used[uid] = name
	if err := a.saveLocked(); err != nil {
		delete(a.apps, name)
		delete(a.used, uid)

		return 0, err
	}

	return uid, nil
}

// UID reports the app's current uid without allocating one.
func (a *uidAllocator) UID(name string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	uid, ok := a.apps[name]

	return uid, ok
}

// chownTree changes ownership of a directory and everything under it, without following symlinks.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return os.Lchown(path, uid, gid)
		}

		return os.Chown(path, uid, gid)
	})
}
