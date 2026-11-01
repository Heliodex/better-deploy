//go:build linux

// Launch-time namespaces for untrusted apps: a user namespace with one mapped host uid, a PID namespace so the app cannot see or signal host processes, a mount namespace with a minimal root that only exposes the bundle and /data, and IPC and UTS namespaces.
// The network namespace is deliberately not included: the app's wasmtime listener must stay reachable from the supervisor's proxy, and moving the listener into a veth-backed namespace is a separate piece of plumbing.
// The per-uid iptables rules remain the network boundary.

package main

import (
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	nsInitArg       = "--wt-ns-init"
	nsPlanEnv       = "WT_INTERNAL_NS_PLAN"
	nsPIDFile       = 3
	nsGoFile        = 4
	defaultTmpfsMB  = 64
	defaultTmpfsDir = 16
)

// nsPlan is the setup the re-exec helper applies before it starts wasmtime.
// Paths are host paths; the helper mounts them into the new root and then pivots into it.
// The helper receives this as JSON in nsPlanEnv.
type nsPlan struct {
	Root, Hostname, AppDir, DataDir, Wasmtime, BinPath string
	Files, LibDirs                                     []string
	TmpfsMB                                            int
	Dynamic, ClearGroups, Seccomp                      bool
	Rlimits                                            []rlimitSpec
}

// nsLaunch carries the helper's process attributes plus the plan.
type nsLaunch struct {
	plan            nsPlan
	hostID, hostGID int
}

// resolveBinary turns WT_WASMTIME into an absolute path, following PATH for a bare name and resolving symlinks so the bind mount points at the real file.
func resolveBinary(name string) (string, error) {
	path := name
	if !strings.ContainsRune(name, filepath.Separator) {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", err
		}
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// elfInterp reports the PT_INTERP string for a dynamic ELF binary.
// A static binary returns an empty string. Non-ELF files report isELF false so the caller can fail closed instead of trying to exec a script in an empty root.
func elfInterp(path string) (interp string, isELF bool, err error) {
	if _, err := os.Stat(path); err != nil {
		return "", false, err
	}
	file, err := elf.Open(path)
	if err != nil {
		return "", false, nil
	}
	defer file.Close()
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(data, 0); err != nil {
			return "", true, err
		}
		return strings.TrimRight(string(data), "\x00"), true, nil
	}
	return "", true, nil
}

// namespaceFiles lists the host files and directories the runtime or its resolver may read.
// Device nodes are omitted because the guest cannot open them through WASI and wasmtime uses getrandom rather than /dev/urandom.
func namespaceFiles(dynamic bool) []string {
	candidates := []string{
		"/etc/group",
		"/etc/hosts",
		"/etc/nsswitch.conf",
		"/etc/passwd",
		"/etc/resolv.conf",
	}
	if dynamic {
		candidates = append(candidates, "/etc/ld.so.cache")
	}
	if _, err := os.Stat("/etc/ssl/certs/ca-certificates.crt"); err == nil {
		candidates = append(candidates, "/etc/ssl/certs/ca-certificates.crt")
	} else if info, err := os.Stat("/etc/ssl/certs"); err == nil && info.IsDir() {
		candidates = append(candidates, "/etc/ssl/certs")
	}
	files := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files
}

// namespaceLibDirs lists the directories a dynamically linked wasmtime needs.
// A static musl build, which is what the runtime image carries, needs none.
func namespaceLibDirs(interp string) []string {
	if interp == "" {
		return nil
	}
	candidates := []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"}
	if dir := filepath.Dir(interp); dir != "." && dir != "/" {
		candidates = append(candidates, dir)
	}
	seen := map[string]bool{}
	dirs := make([]string, 0, len(candidates))
	for _, dir := range candidates {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// namespaceIDs picks the host uid mapped to container uid 0.
// Untrusted apps use their allocated uid; trusted apps may use namespaces only when the supervisor is not root, because there is no per-app uid to map to otherwise.
func (s *supervisor) namespaceIDs(iso *launchIsolation) (int, int, bool, error) {
	if iso != nil && iso.uid > 0 {
		if os.Geteuid() != 0 {
			return 0, 0, false, fmt.Errorf("untrusted app namespaces need root")
		}
		return iso.uid, iso.gid, true, nil
	}
	if os.Geteuid() == 0 {
		return 0, 0, false, fmt.Errorf("app namespaces in trusted mode need a non-root supervisor; use untrusted mode or run the supervisor as a non-root user")
	}
	return os.Geteuid(), os.Getegid(), false, nil
}

// newNamespaceRoot creates the directory that becomes the helper's tmpfs root.
// The helper runs as the supervisor's uid until it has mounted everything, so the directory does not need the app uid's ownership.
// The parent removes it when the process exits.
func (s *supervisor) newNamespaceRoot(a *app) (string, error) {
	base := filepath.Join(s.cfg.stateDir, ".ns")
	mode := os.FileMode(0o700)
	if s.cfg.untrusted() {
		// The helper is root and needs to traverse .ns. It is not listable.
		mode = 0o711
	}
	if err := os.MkdirAll(base, mode); err != nil {
		return "", fmt.Errorf("namespace root: %w", err)
	}
	if err := os.Chmod(base, mode); err != nil {
		return "", fmt.Errorf("namespace root permissions: %w", err)
	}
	root, err := os.MkdirTemp(base, a.name+"-")
	if err != nil {
		return "", fmt.Errorf("create namespace root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("namespace root permissions: %w", err)
	}
	return root, nil
}

// buildNamespacePlan resolves the runtime binary and the files the app needs inside its minimal root.
// A static wasmtime needs only the binary; a dynamic one also gets its interpreter directory and the usual library directories.
func (s *supervisor) buildNamespacePlan(a *app, root string, clearGroups bool) (nsPlan, error) {
	wasmtime, err := resolveBinary(s.cfg.wasmtime)
	if err != nil {
		return nsPlan{}, fmt.Errorf("resolve wasmtime: %w", err)
	}
	interp, isELF, err := elfInterp(wasmtime)
	if err != nil {
		return nsPlan{}, fmt.Errorf("inspect %s: %w", wasmtime, err)
	}
	if !isELF {
		return nsPlan{}, fmt.Errorf("app namespaces need an ELF wasmtime binary, %s is not ELF", wasmtime)
	}

	appDir, err := filepath.Abs(a.dir)
	if err != nil {
		return nsPlan{}, fmt.Errorf("app dir: %w", err)
	}
	dataDir := filepath.Join(s.cfg.stateDir, a.name)
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return nsPlan{}, fmt.Errorf("data dir %s is not a directory", dataDir)
	}

	dynamic := interp != ""
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	var limits []rlimitSpec
	if s.cfg.untrusted() {
		limits = rlimitPlan(cfg)
	}
	return nsPlan{
		Root:        root,
		Hostname:    a.name,
		AppDir:      appDir,
		DataDir:     dataDir,
		Wasmtime:    wasmtime,
		BinPath:     "/bin/wasmtime",
		Dynamic:     dynamic,
		Files:       namespaceFiles(dynamic),
		LibDirs:     namespaceLibDirs(interp),
		TmpfsMB:     defaultTmpfsMB,
		ClearGroups: clearGroups,
		Seccomp:     s.cfg.appSeccomp,
		Rlimits:     limits,
	}, nil
}

// namespaceSysProcAttr creates the user, PID, mount, IPC and UTS namespaces and maps container uid/gid 0 to the app's host uid/gid.
// The helper stays root inside the namespace until it has finished mounting, then switches to container 0 and drops capabilities.
func namespaceSysProcAttr(hostID, hostGID int, allowSetgroups bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Pdeathsig:                  syscall.SIGTERM,
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: hostID, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: hostGID, Size: 1}},
		GidMappingsEnableSetgroups: allowSetgroups,
	}
}

// resetNamespaceRoots removes stale roots from a previous supervisor.
// The mounts themselves live in the dead helpers' namespaces, so the host paths are only empty directories.
func resetNamespaceRoots(stateDir string, untrusted bool) error {
	base := filepath.Join(stateDir, ".ns")
	if err := os.RemoveAll(base); err != nil {
		return fmt.Errorf("reset namespace roots: %w", err)
	}
	mode := os.FileMode(0o700)
	if untrusted {
		mode = 0o711
	}
	if err := os.MkdirAll(base, mode); err != nil {
		return fmt.Errorf("create namespace roots: %w", err)
	}
	return os.Chmod(base, mode)
}

// watchNamespaceChild reads the host pid that the helper writes to the pid pipe.
// Resource sampling and app info use the wasmtime pid, not the helper.
func watchNamespaceChild(a *app, cmd *exec.Cmd, reader *os.File) {
	defer reader.Close()
	data, _ := io.ReadAll(io.LimitReader(reader, 64))
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	a.mu.Lock()
	if a.cmd == cmd && !a.exited {
		a.workPID = pid
	}
	a.mu.Unlock()
}

// prepareNamespaces builds the namespace plan for one launch attempt.
func (s *supervisor) prepareNamespaces(a *app, iso *launchIsolation) (*nsLaunch, error) {
	hostID, hostGID, allowSetgroups, err := s.namespaceIDs(iso)
	if err != nil {
		return nil, err
	}
	root, err := s.newNamespaceRoot(a)
	if err != nil {
		return nil, err
	}
	plan, err := s.buildNamespacePlan(a, root, allowSetgroups)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return &nsLaunch{plan: plan, hostID: hostID, hostGID: hostGID}, nil
}
