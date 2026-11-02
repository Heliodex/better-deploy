// Launch-time isolation: per-app uid, /data ownership and cgroup limits.

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// isolationPlan owns every per-launch resource outside the process itself.
type isolationPlan struct {
	app            *app
	uid, gid       int
	cgroup, nsRoot string
	ns             *nsLaunch
	closed         bool
}

// prepareIsolation allocates the app uid, /data ownership, quota, cgroup and namespaces before the process starts.
// Trusted apps keep the single-user behaviour; cgroup failures are only warnings there.
func (s *supervisor) prepareIsolation(a *app, cfg appConfig) (*isolationPlan, error) {
	plan := &isolationPlan{app: a}

	dataDir := filepath.Join(s.cfg.stateDir, a.name)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	if s.cfg.untrusted() {
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf("untrusted mode requires root to set the app uid; running as uid %d", os.Geteuid())
		}

		if s.uids == nil {
			return nil, errors.New("untrusted mode has no uid allocator")
		}

		if err := ensureDirTraversable(s.cfg.appsDir); err != nil {
			return nil, fmt.Errorf("apps directory: %w", err)
		}

		if err := ensureDirTraversable(s.cfg.stateDir); err != nil {
			return nil, fmt.Errorf("state directory: %w", err)
		}

		uid, err := s.uids.Allocate(a.name)
		if err != nil {
			return nil, fmt.Errorf("allocate app uid: %w", err)
		}

		plan.uid, plan.gid = uid, uid
		if err := chownTree(dataDir, uid, uid); err != nil {
			return nil, fmt.Errorf("chown /data to uid %d: %w", uid, err)
		}

		if err := ensureAppReadable(a.dir); err != nil {
			return nil, fmt.Errorf("make app readable by uid %d: %w", uid, err)
		}

		a.mu.Lock()
		a.uid = uid
		a.mu.Unlock()

		if err := s.quota.apply(uid, int64(cfg.DiskMB)<<20, maxAppEntries); err != nil {
			return nil, fmt.Errorf("data quota: %w", err)
		}

		if s.cgroups == nil {
			return nil, errors.New("untrusted mode has no cgroup manager")
		}

		path, err := s.cgroups.create(a.name, cfg)
		if err != nil {
			return nil, fmt.Errorf("cgroup: %w", err)
		}

		plan.cgroup = path
	}

	if s.cfg.appNamespaces {
		ns, err := s.prepareNamespaces(a, plan)
		if err != nil {
			plan.close(s)
			return nil, err
		}

		plan.ns = ns
		plan.nsRoot = ns.plan.Root
	}

	return plan, nil
}

// attachProcess adds a started process to the plan's cgroup.
func (p *isolationPlan) attachProcess(s *supervisor, pid int) error {
	if p == nil || p.cgroup == "" || s.cgroups == nil {
		return nil
	}

	return s.cgroups.addPID(p.app.name, pid)
}

// activate installs the firewall rules that need the app's listening port.
func (p *isolationPlan) activate(s *supervisor, cfg appConfig) error {
	if cfg.Network != "storage" {
		return nil
	}

	if !s.cfg.untrusted() {
		log.Printf("app %s: network storage in trusted mode has no per-app uid, so inherit-network stays unrestricted", p.app.name)
		return nil
	}

	if err := s.applyEgress(p.app); err != nil {
		return fmt.Errorf("egress rules: %w", err)
	}

	return nil
}

// close removes the plan's firewall, cgroup and namespace resources. It is safe to call more than once.
func (p *isolationPlan) close(s *supervisor) {
	if p == nil || p.closed {
		return
	}

	p.closed = true
	s.removeAppEgress(p.app)
	s.removeAppCgroup(p.app)

	if p.nsRoot != "" {
		_ = os.RemoveAll(p.nsRoot)
	}
}

// ensureDirTraversable adds execute bits to a parent directory without granting read or write access.
// The app UID must be able to traverse WT_APPS_DIR and WT_STATE_DIR to reach its bundle and /data preopen.
func ensureDirTraversable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	mode := info.Mode().Perm()

	want := mode | 0o111
	if want == mode {
		return nil
	}

	if err := os.Chmod(dir, want); err != nil {
		return fmt.Errorf("chmod %s to %o: %w", dir, want, err)
	}

	return nil
}

// ensureAppReadable makes a bundle traversable and readable by the app uid.
// Directories become 0755; regular files become 0644 while keeping any execute bits they already had.
// The root app.json holds env secrets and is made 0600 instead.
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
			if path == filepath.Join(root, "app.json") {
				return os.Chmod(path, 0o600)
			}

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
		return errors.New("no egress firewall manager")
	}

	a.mu.Lock()
	uid, port := a.uid, a.port
	egress := append([]egressRule(nil), a.cfg.Egress...)
	a.mu.Unlock()

	return s.egress.add(a.name, uid, port, egress...)
}
