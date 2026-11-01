//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSeccompFilterShape(t *testing.T) {
	arch, err := seccompArch()
	if err != nil {
		t.Skip(err)
	}
	denied := deniedSyscalls()
	filter := seccompFilter(arch, denied)
	if len(filter) < len(denied)*2 {
		t.Fatalf("filter has %d instructions for %d denied syscalls", len(filter), len(denied))
	}
	if filter[0].K != 4 || filter[1].K != arch || filter[2].K != seccompRetKillProcess {
		t.Errorf("arch guard = %+v", filter[:3])
	}
	if last := filter[len(filter)-1]; last.K != seccompRetAllow {
		t.Errorf("last instruction = %+v, want allow", last)
	}
	for _, nr := range denied {
		found := false
		for _, instruction := range filter {
			if instruction.Code == unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K && instruction.K == uint32(nr) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("syscall %d is not denied", nr)
		}
	}
	prctlGuard := false
	for i, instruction := range filter {
		if instruction.K != uint32(unix.SYS_PRCTL) {
			continue
		}
		if i+2 < len(filter) && filter[i+2].K == uint32(unix.PR_SET_SECCOMP) {
			prctlGuard = true
		}
	}
	if !prctlGuard {
		t.Error("filter does not guard prctl(PR_SET_SECCOMP)")
	}
}

func TestSeccompInstallHelper(t *testing.T) {
	if os.Getenv("WT_SECCOMP_HELPER") != "1" {
		t.Skip("seccomp install helper")
	}
	if err := installSeccomp(); err != nil {
		t.Fatal(err)
	}
	err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, 0, 0, 0)
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("installing a second filter = %v, want EPERM", err)
	}
}

func TestSeccompInstall(t *testing.T) {
	if os.Getenv("WT_SECCOMP_TEST") != "1" {
		t.Skip("set WT_SECCOMP_TEST=1 to install a seccomp filter in a child")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSeccompInstallHelper$")
	cmd.Env = append(os.Environ(), "WT_SECCOMP_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seccomp helper: %v\n%s", err, out)
	}
}
