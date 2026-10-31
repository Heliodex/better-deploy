// Launch-time isolation: per-app uid, /data ownership and cgroup limits.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

type launchIsolation struct {
	uid, gid int
	cgroup   string
}

// prepareLaunch gives an untrusted app its identity and cgroup before the process starts.
// Trusted apps keep the old single-user behaviour; cgroup failures are only warnings there.
func (s *supervisor) prepareLaunch(a *app, cfg appConfig) (*launchIsolation, error) {
	iso := &launchIsolation{}
	dataDir := filepath.Join(s.cfg.stateDir, a.name)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	if s.cfg.untrusted() {
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf("untrusted mode requires root to set the app uid; running as uid %d", os.Geteuid())
		}
		if s.uids == nil {
			return nil, fmt.Errorf("untrusted mode has no uid allocator")
		}
		uid, err := s.uids.Allocate(a.name)
		if err != nil {
			return nil, fmt.Errorf("allocate app uid: %w", err)
		}
		iso.uid, iso.gid = uid, uid
		if err := chownTree(dataDir, uid, uid); err != nil {
			return nil, fmt.Errorf("chown /data to uid %d: %w", uid, err)
		}
		if err := ensureAppReadable(a.dir); err != nil {
			return nil, fmt.Errorf("make app readable by uid %d: %w", uid, err)
		}
		a.mu.Lock()
		a.uid = uid
		a.mu.Unlock()

		if s.cgroups == nil {
			return nil, fmt.Errorf("untrusted mode has no cgroup manager")
		}
		path, err := s.cgroups.create(a.name, cfg)
		if err != nil {
			return nil, fmt.Errorf("cgroup: %w", err)
		}
		iso.cgroup = path
	}
	return iso, nil
}

// ensureAppReadable makes a bundle traversable and readable by the app uid.
// Directories become 0755; regular files become 0644 while keeping any execute bits they already had.
func ensureAppReadable(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("app path %s is not a directory", root)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.Chmod(path, 0o755)
		case mode.IsRegular():
			perm := os.FileMode(0o644)
			if mode.Perm()&0o111 != 0 {
				perm = 0o755
			}
			return os.Chmod(path, perm)
		default:
			return nil
		}
	})
}

// releaseIsolation removes resources that belong to one launch attempt. The process must already be gone.
func (s *supervisor) releaseIsolation(a *app, iso *launchIsolation) {
	if a == nil {
		return
	}
	s.removeAppEgress(a)
	if iso != nil && iso.cgroup != "" {
		s.removeAppCgroup(a)
	}
}

// releaseAppIsolation removes per-app resources on stop, using the app name so a stopped app leaves nothing behind.
func (s *supervisor) releaseAppIsolation(a *app) {
	if a == nil {
		return
	}
	s.removeAppEgress(a)
	s.removeAppCgroup(a)
}

func (s *supervisor) removeAppEgress(a *app) {
	if a == nil || s.egress == nil {
		return
	}
	if err := s.egress.remove(a.name); err != nil {
		log.Printf("app %s: remove egress rules: %v", a.name, err)
	}
}

func (s *supervisor) removeAppCgroup(a *app) {
	if a == nil || s.cgroups == nil {
		return
	}
	if err := s.cgroups.remove(a.name); err != nil {
		log.Printf("app %s: remove cgroup: %v", a.name, err)
	}
}

// applyEgress installs the app's owner-match rules now that its loopback port is known.
func (s *supervisor) applyEgress(a *app) error {
	if s.egress == nil {
		return fmt.Errorf("no egress firewall manager")
	}
	a.mu.Lock()
	uid, port := a.uid, a.port
	a.mu.Unlock()
	return s.egress.add(a.name, uid, port)
}

// appCredential builds the SysProcAttr credential for an untrusted app. A nil result keeps the supervisor's own identity.
func appCredential(iso *launchIsolation) *syscall.Credential {
	if iso == nil || iso.uid == 0 {
		return nil
	}
	// Explicitly empty supplementary groups so the app cannot inherit root's group memberships.
	return &syscall.Credential{Uid: uint32(iso.uid), Gid: uint32(iso.gid), Groups: []uint32{}}
}
