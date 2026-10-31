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
		return iso, nil
	}

	if s.cgroups != nil {
		path, err := s.cgroups.create(a.name, cfg)
		if err != nil {
			log.Printf("app %s: cgroup limits unavailable (trusted mode): %v", a.name, err)
		} else {
			iso.cgroup = path
		}
	}
	return iso, nil
}

// releaseIsolation removes resources that belong to one launch attempt. The process must already be gone.
func (s *supervisor) releaseIsolation(a *app, iso *launchIsolation) {
	if iso == nil || iso.cgroup == "" || s.cgroups == nil {
		return
	}
	if err := s.cgroups.remove(a.name); err != nil {
		log.Printf("app %s: remove cgroup: %v", a.name, err)
	}
}

// releaseAppIsolation removes per-app resources on stop, using the app name so a stopped app leaves nothing behind.
func (s *supervisor) releaseAppIsolation(a *app) {
	if a == nil || s.cgroups == nil {
		return
	}
	if err := s.cgroups.remove(a.name); err != nil {
		log.Printf("app %s: remove cgroup: %v", a.name, err)
	}
}

// appCredential builds the SysProcAttr credential for an untrusted app. A nil result keeps the supervisor's own identity.
func appCredential(iso *launchIsolation) *syscall.Credential {
	if iso == nil || iso.uid == 0 {
		return nil
	}
	return &syscall.Credential{Uid: uint32(iso.uid), Gid: uint32(iso.gid)}
}
