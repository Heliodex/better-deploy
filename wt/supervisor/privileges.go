package main

import (
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// dropPrivileges chowns the state volume and drops to the app-running user.
// A Kubernetes PVC arrives owned by root even though the image's process user is 10001,
// so the entrypoint starts as root only long enough to fix ownership. Set
// WT_DROP_PRIVILEGES=1 to enable this; a container already running as 10001 is untouched.
func dropPrivileges() {
	if os.Getenv("WT_DROP_PRIVILEGES") != "1" || os.Geteuid() != 0 {
		return
	}

	uid := int(envInt64("WT_RUN_UID", 10001))
	gid := int(envInt64("WT_RUN_GID", 10001))
	appsDir := env("WT_APPS_DIR", "./apps")

	stateDir := env("WT_STATE_DIR", "./state")
	for _, dir := range []string{appsDir, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("drop privileges: create %s: %v", dir, err)
		}

		if err := chownTree(dir, uid, gid); err != nil {
			log.Fatalf("drop privileges: chown %s: %v", dir, err)
		}
	}
	// TMPDIR is the volume root in the image, so it must be writable by the app user too.
	parent := filepath.Dir(stateDir)
	if parent != "." && parent != "/" {
		if err := os.Chown(parent, uid, gid); err != nil {
			log.Fatalf("drop privileges: chown %s: %v", parent, err)
		}
	}

	if err := syscall.Setgroups([]int{gid}); err != nil {
		log.Fatalf("drop privileges: setgroups: %v", err)
	}

	if err := syscall.Setgid(gid); err != nil {
		log.Fatalf("drop privileges: setgid: %v", err)
	}

	if err := syscall.Setuid(uid); err != nil {
		log.Fatalf("drop privileges: setuid: %v", err)
	}
}
