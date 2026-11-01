//go:build linux

// A defensive seccomp filter for app processes.
// This is a denylist: the primary boundary is the user, mount and PID namespace plus the dropped capabilities.
// Seccomp removes the remaining ways to attack the shared kernel or replace the filter itself without maintaining a fragile allowlist for every syscall wasmtime may use.

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompRetKillProcess = 0x80000000
	seccompRetAllow       = 0x7fff0000
	seccompRetErrno       = 0x00050000
	x32SyscallBit         = 0x40000000
)

// commonDeniedSyscalls are dangerous syscalls present on both amd64 and arm64.
// Arch-specific extras live in seccomp_linux_<arch>.go.
var commonDeniedSyscalls = []uintptr{
	unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV,
	unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_KCMP,
	unix.SYS_BPF,
	unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL,
	unix.SYS_ADD_KEY,
	unix.SYS_REQUEST_KEY,
	unix.SYS_MOUNT,
	unix.SYS_UMOUNT2,
	unix.SYS_PIVOT_ROOT,
	unix.SYS_CHROOT,
	unix.SYS_SETNS,
	unix.SYS_UNSHARE,
	unix.SYS_REBOOT,
	unix.SYS_KEXEC_LOAD,
	unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_INIT_MODULE,
	unix.SYS_FINIT_MODULE,
	unix.SYS_DELETE_MODULE,
	unix.SYS_SWAPON,
	unix.SYS_SWAPOFF,
	unix.SYS_ACCT,
	unix.SYS_SYSLOG,
	unix.SYS_QUOTACTL,
	unix.SYS_SETTIMEOFDAY,
	unix.SYS_CLOCK_SETTIME,
	unix.SYS_CLOCK_ADJTIME,
	unix.SYS_ADJTIMEX,
	unix.SYS_OPEN_BY_HANDLE_AT,
	unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_FANOTIFY_INIT,
	unix.SYS_LOOKUP_DCOOKIE,
	unix.SYS_NFSSERVCTL,
	unix.SYS_SECCOMP,
	unix.SYS_MOUNT_SETATTR,
	unix.SYS_OPEN_TREE,
	unix.SYS_MOVE_MOUNT,
	unix.SYS_FSOPEN,
	unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT,
	unix.SYS_FSPICK,
}

func seccompArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	default:
		return 0, fmt.Errorf("seccomp is not supported on %s", runtime.GOARCH)
	}
}

// seccompFilter builds a classic BPF program that kills a foreign ABI, rejects x32 syscalls on amd64, denies replacing the filter through prctl, then returns EPERM for each denied syscall and allows everything else.
func seccompFilter(arch uint32, denied []uintptr) []unix.SockFilter {
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetKillProcess},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	if runtime.GOARCH == "amd64" {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: x32SyscallBit},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetErrno | uint32(unix.EPERM)},
		)
	}
	// Deny prctl(PR_SET_SECCOMP) and prctl(PR_SET_MM); a non-prctl syscall skips all five following instructions.
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 5, K: uint32(unix.SYS_PRCTL)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.PR_SET_SECCOMP)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetErrno | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.PR_SET_MM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetErrno | uint32(unix.EPERM)},
	)
	for _, nr := range denied {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(nr)},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetErrno | uint32(unix.EPERM)},
		)
	}
	return append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: seccompRetAllow})
}

func installSeccomp() error {
	arch, err := seccompArch()
	if err != nil {
		return err
	}
	filter := seccompFilter(arch, deniedSyscalls())
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}
