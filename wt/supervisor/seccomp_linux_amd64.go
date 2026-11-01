//go:build linux && amd64

package main

import "golang.org/x/sys/unix"

func deniedSyscalls() []uintptr {
	denied := append([]uintptr{}, commonDeniedSyscalls...)
	return append(denied,
		unix.SYS_MODIFY_LDT,
		unix.SYS_IOPL,
		unix.SYS_IOPERM,
		unix.SYS__SYSCTL,
	)
}
