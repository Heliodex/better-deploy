// Supervisor handover: exec a new binary while the app runner keeps the apps.

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	handoverEnv  = "WT_SUPERVISOR_HANDOVER"
	storageFDEnv = "WT_SUPERVISOR_STORAGE_FD"
	fetchFDEnv   = "WT_SUPERVISOR_FETCH_FD"
	runnerFDEnv  = "WT_SUPERVISOR_RUNNER_FD"
)

// inheritedListener rebuilds a listener from an FD passed by a handover.
// The original FD is closed after net.FileListener duplicates it.
func inheritedListener(key string) (net.Listener, bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return nil, false, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 0 {
		return nil, false, fmt.Errorf("%s=%q is not an FD", key, raw)
	}
	file := os.NewFile(uintptr(fd), key)
	if file == nil {
		return nil, false, fmt.Errorf("%s=%q is not an open FD", key, raw)
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", key, err)
	}
	return listener, true, nil
}

func listenerFile(listener net.Listener) (*os.File, error) {
	switch value := listener.(type) {
	case *net.TCPListener:
		return value.File()
	case *net.UnixListener:
		return value.File()
	default:
		return nil, fmt.Errorf("unsupported listener %T", listener)
	}
}

func clearCloseOnExec(file *os.File) error {
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
		return fmt.Errorf("clear close-on-exec on %s: %w", file.Name(), err)
	}
	return nil
}

// startRunner connects to an existing runner during a handover or starts a new one.
// The runner owns the app processes; its liveness pipe ties it to this supervisor process.
func (s *supervisor) startRunner() error {
	socket := runnerSocketPath(s.cfg.stateDir)
	handover := os.Getenv(handoverEnv) == "1"
	if handover {
		if raw := os.Getenv(runnerFDEnv); raw != "" {
			if fd, err := strconv.Atoi(raw); err == nil && fd >= 0 {
				s.runnerLiveness = os.NewFile(uintptr(fd), "runner-liveness")
			}
		}
		client, snapshot, pid, err := dialRunner(socket, 5*time.Second)
		if err == nil {
			s.runner = client
			s.runnerSnapshot = snapshot
			go s.reapRunner(pid)
			return nil
		}
		log.Printf("runner handover: %v; starting a new runner", err)
		if s.runnerLiveness != nil {
			_ = s.runnerLiveness.Close()
			s.runnerLiveness = nil
		}
		for range 50 {
			if _, err := os.Stat(socket); os.IsNotExist(err) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		// A runner from a supervisor that crashed should have exited when its
		// liveness pipe closed. If one still answers, stop it rather than
		// adopting stale apps into a cold start.
		if client, _, _, err := dialRunner(socket, 500*time.Millisecond); err == nil {
			_ = client.Shutdown()
			client.Close()
			for range 50 {
				if _, err := os.Stat(socket); os.IsNotExist(err) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	_ = os.Remove(socket)

	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("runner liveness pipe: %w", err)
	}
	cmd := exec.Command("/proc/self/exe", runnerArg)
	cmd.Env = append(minimalEnv(), runnerSocketEnv+"="+socket, runnerLivenessEnv+"=3")
	cmd.ExtraFiles = []*os.File{read}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return fmt.Errorf("start runner: %w", err)
	}
	_ = read.Close()
	s.runnerLiveness = write

	client, snapshot, pid, err := dialRunner(socket, 5*time.Second)
	if err != nil {
		_ = write.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("runner: %w", err)
	}
	s.runner = client
	s.runnerSnapshot = snapshot
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("runner: %v", err)
		}
	}()
	_ = pid
	return nil
}

func (s *supervisor) reapRunner(pid int) {
	if pid <= 0 {
		return
	}
	var status syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			log.Printf("runner pid %d: %v", pid, err)
		}
		return
	}
}

// handover writes the listener FDs and execs the new supervisor binary.
// The app runner, its liveness pipe and the storage and app-fetch listeners survive the exec; public and admin listeners rebind in the new process.
func (s *supervisor) handover() error {
	if s.runner == nil || s.runnerLiveness == nil {
		return fmt.Errorf("no app runner to hand over")
	}
	storageFile, err := listenerFile(s.storage.listener)
	if err != nil {
		return fmt.Errorf("storage listener: %w", err)
	}
	fetchFile, err := listenerFile(s.fetch.listener)
	if err != nil {
		_ = storageFile.Close()
		return fmt.Errorf("app fetch listener: %w", err)
	}
	files := []*os.File{storageFile, fetchFile}
	env := make([]string, 0, len(os.Environ())+4)
	for _, pair := range os.Environ() {
		key, _, _ := strings.Cut(pair, "=")
		switch key {
		case handoverEnv, storageFDEnv, fetchFDEnv, runnerFDEnv:
			continue
		}
		env = append(env, pair)
	}
	add := func(key string, file *os.File) error {
		if err := clearCloseOnExec(file); err != nil {
			return err
		}
		env = append(env, key+"="+strconv.Itoa(int(file.Fd())))
		return nil
	}
	if err := add(storageFDEnv, storageFile); err != nil {
		return err
	}
	if err := add(fetchFDEnv, fetchFile); err != nil {
		return err
	}
	if err := add(runnerFDEnv, s.runnerLiveness); err != nil {
		return err
	}
	env = append(env, handoverEnv+"=1")

	executable := os.Args[0]
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("find supervisor binary: %w", err)
		}
	}
	if !strings.ContainsRune(executable, filepath.Separator) {
		resolved, err := exec.LookPath(executable)
		if err != nil {
			return fmt.Errorf("find supervisor binary %q: %w", executable, err)
		}
		executable = resolved
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve supervisor binary: %w", err)
	}
	s.stopping.Store(true)
	log.Printf("handover: exec %s with %d app(s)", executable, len(s.allApps()))
	if err := syscall.Exec(executable, os.Args, env); err != nil {
		s.stopping.Store(false)
		return fmt.Errorf("exec %s: %w", executable, err)
	}
	runtime.KeepAlive(files)
	return nil
}
