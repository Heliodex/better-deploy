//go:build linux

// nsInitMain is the re-exec helper that runs inside the new namespaces.
// It mounts the minimal root, pivots into it, drops capabilities, installs the seccomp filter and then runs wasmtime as a child.
// The helper stays as PID 1 so it can forward signals to the child and reap it; otherwise PID 1 would ignore SIGTERM and the supervisor's graceful stop would time out.

package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func nsFatal(err error) {
	fmt.Fprintf(os.Stderr, "wt ns-init: %v\n", err)
	os.Exit(125)
}

func readNSPlan() (nsPlan, error) {
	var plan nsPlan

	raw := os.Getenv(nsPlanEnv)
	if raw == "" {
		return plan, fmt.Errorf("%s is not set", nsPlanEnv)
	}

	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return plan, fmt.Errorf("parse namespace plan: %w", err)
	}

	if plan.Root == "" || plan.AppDir == "" || plan.DataDir == "" || plan.Wasmtime == "" || !strings.HasPrefix(plan.BinPath, "/") {
		return plan, errors.New("namespace plan is incomplete")
	}

	if plan.TmpfsMB < 1 || plan.TmpfsMB > 1024 {
		return plan, fmt.Errorf("invalid tmpfs size %d", plan.TmpfsMB)
	}

	return plan, nil
}

// insideRoot joins a host path into the new root without letting an absolute path discard the root prefix.
func insideRoot(root, path string) string {
	return filepath.Join(root, strings.TrimPrefix(path, "/"))
}

func bindDir(src, dst string, readOnly bool) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}

	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV)
	if readOnly {
		flags |= unix.MS_RDONLY
	}

	return unix.Mount("", dst, "", flags, "")
}

func bindFile(src, dst string, readOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	file, err := os.OpenFile(dst, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return err
	}

	_ = file.Close()

	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return err
	}

	if readOnly {
		return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "")
	}

	return nil
}

func bindPath(src, dst string, readOnly bool) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return bindDir(src, dst, readOnly)
	}

	return bindFile(src, dst, readOnly)
}

// setupNamespace builds the new root and pivots into it.
// All mounts happen in the helper's private mount namespace, so the host never sees them.
func setupNamespace(plan *nsPlan) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	rootFlags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if err := unix.Mount("tmpfs", plan.Root, "tmpfs", rootFlags, fmt.Sprintf("size=%dm,mode=0755", plan.TmpfsMB)); err != nil {
		return fmt.Errorf("mount root tmpfs: %w", err)
	}

	for _, dir := range []string{"app", "bin", "data", "etc", "proc", "tmp"} {
		if err := os.MkdirAll(insideRoot(plan.Root, "/"+dir), 0o755); err != nil {
			return fmt.Errorf("create /%s: %w", dir, err)
		}
	}

	if err := bindPath(plan.AppDir, insideRoot(plan.Root, "/app"), true); err != nil {
		return fmt.Errorf("mount /app: %w", err)
	}

	if err := bindPath(plan.DataDir, insideRoot(plan.Root, "/data"), false); err != nil {
		return fmt.Errorf("mount /data: %w", err)
	}

	if err := bindFile(plan.Wasmtime, insideRoot(plan.Root, plan.BinPath), true); err != nil {
		return fmt.Errorf("mount %s: %w", plan.BinPath, err)
	}

	for _, dir := range plan.LibDirs {
		if err := bindPath(dir, insideRoot(plan.Root, dir), true); err != nil {
			return fmt.Errorf("mount %s: %w", dir, err)
		}
	}

	for _, file := range plan.Files {
		if err := bindPath(file, insideRoot(plan.Root, file), true); err != nil {
			return fmt.Errorf("mount %s: %w", file, err)
		}
	}

	if err := unix.Mount("proc", insideRoot(plan.Root, "/proc"), "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}

	tmpFlags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("tmpfs", insideRoot(plan.Root, "/tmp"), "tmpfs", tmpFlags, fmt.Sprintf("size=%dm,mode=1777", defaultTmpfsDir)); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}

	oldRoot := insideRoot(plan.Root, "/.oldroot")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		return fmt.Errorf("create old root: %w", err)
	}

	if err := unix.PivotRoot(plan.Root, oldRoot); err != nil {
		return fmt.Errorf("pivot root: %w", err)
	}

	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after pivot: %w", err)
	}

	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}

	if err := unix.Rmdir("/.oldroot"); err != nil {
		return fmt.Errorf("remove old root: %w", err)
	}

	if err := unix.Sethostname([]byte(plan.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}

	return nil
}

func dropBoundingCaps() error {
	for cap := range 64 {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("drop capability %d: %w", cap, err)
		}
	}

	return nil
}

func clearCaps() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}

	var empty [2]unix.CapUserData

	_, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&empty[0])), 0, 0, 0, 0)
	if errno == 0 {
		return nil
	}
	// AllThreadsSyscall returns ENOTSUP in cgo binaries.
	// The goroutine is locked to its thread, so clearing that thread is enough for the child to inherit no capabilities.
	if err := unix.Capset(&header, &empty[0]); err == nil {
		return nil
	}
	// setuid to a non-zero uid may already have cleared the sets.
	// Verify before treating an EPERM from capset as a setup failure.
	var current [2]unix.CapUserData
	if err := unix.Capget(&header, &current[0]); err == nil {
		if current[0] == (unix.CapUserData{}) && current[1] == (unix.CapUserData{}) {
			return nil
		}
	}

	return fmt.Errorf("clear capabilities: %w", errno)
}

// applyPlanRlimits lowers the helper's limits before it starts wasmtime, so the child inherits them at exec time.
// The supervisor cannot apply them after start because the helper may already have launched the runtime.
func applyPlanRlimits(plan *nsPlan) error {
	for _, spec := range plan.Rlimits {
		limit := &unix.Rlimit{Cur: spec.Value, Max: spec.Value}
		if err := unix.Setrlimit(spec.Resource, limit); err != nil {
			return fmt.Errorf("set %s=%d: %w", spec.Name, spec.Value, err)
		}
	}

	return nil
}

// switchToApp drops the setup capabilities and maps the process to container uid/gid 0, which the user namespace maps to the app's host uid/gid.
// The helper keeps no capabilities and cannot remount or create namespaces.
func switchToApp(plan *nsPlan) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}

	if err := dropBoundingCaps(); err != nil {
		return err
	}

	if plan.ClearGroups {
		if err := syscall.Setgroups(nil); err != nil {
			return fmt.Errorf("clear supplementary groups: %w", err)
		}
	}

	if err := syscall.Setresgid(0, 0, 0); err != nil {
		return fmt.Errorf("set gid: %w", err)
	}

	if err := syscall.Setresuid(0, 0, 0); err != nil {
		return fmt.Errorf("set uid: %w", err)
	}

	if err := clearCaps(); err != nil {
		return err
	}

	return applyPlanRlimits(plan)
}

// childEnv passes the app environment to wasmtime without the internal plan.
func childEnv() []string {
	env := os.Environ()

	out := env[:0]
	for _, pair := range env {
		if strings.HasPrefix(pair, nsPlanEnv+"=") {
			continue
		}

		out = append(out, pair)
	}

	return out
}

// runApp starts the runtime as PID 2 in the namespace and stays alive as PID 1 to forward signals.
// The child's host pid is written to the pid pipe so the supervisor can report and sample the wasmtime process rather than the helper.
func runApp(plan *nsPlan, args []string) int {
	cmd := exec.Command(plan.BinPath, args...)
	cmd.Env = childEnv()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "wt ns-init: start %s: %v\n", plan.BinPath, err)
		return 125
	}

	if file := os.NewFile(nsPIDFile, "wt-pid"); file != nil {
		_, _ = fmt.Fprintf(file, "%d\n", cmd.Process.Pid)
		_ = file.Close()
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)

	done := make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-signals:
				if value, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(-cmd.Process.Pid, value)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()

	close(done)
	signal.Stop(signals)

	if err == nil {
		return 0
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}

		return exitErr.ExitCode()
	}

	return 125
}

// waitForStart blocks until the supervisor has added the helper to its cgroup.
// The runtime starts only after that, so it inherits the cgroup membership at fork time instead of racing the supervisor's addPID call.
func waitForStart() error {
	file := os.NewFile(nsGoFile, "wt-go")
	if file == nil {
		return errors.New("start pipe is missing")
	}
	defer file.Close()

	var buf [1]byte
	if _, err := io.ReadFull(file, buf[:]); err != nil {
		return fmt.Errorf("wait for supervisor: %w", err)
	}

	return nil
}

func nsInitMain(args []string) {
	// Seccomp filters and the credential switch are per-thread.
	// Locking this goroutine keeps the child on the thread that installed the filter and dropped the capabilities, so wasmtime inherits both.
	runtime.LockOSThread()

	plan, err := readNSPlan()
	if err != nil {
		nsFatal(err)
	}

	if err := setupNamespace(&plan); err != nil {
		nsFatal(err)
	}

	if err := switchToApp(&plan); err != nil {
		nsFatal(err)
	}

	if plan.Seccomp {
		if err := installSeccomp(); err != nil {
			nsFatal(err)
		}
	}

	if err := waitForStart(); err != nil {
		nsFatal(err)
	}

	os.Exit(runApp(&plan, args))
}
