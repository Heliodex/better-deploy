//go:build linux && arm64

package main

func deniedSyscalls() []uintptr {
	return append([]uintptr{}, commonDeniedSyscalls...)
}
