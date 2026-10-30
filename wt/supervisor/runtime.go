// Process and environment helpers shared by launching and deploying apps.

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func freePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func minimalEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}
	env := make([]string, 0, len(keep))
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// maxLaunchLogBytes caps how much of a wasmtime process's output is kept for failed-launch errors.
const maxLaunchLogBytes = 8 << 10

// logRing keeps the tail of a process's output for failed-launch errors, so deploys report why wasmtime exited.
type logRing struct {
	mu  sync.Mutex
	buf []byte
}

func newLogRing() *logRing {
	return &logRing{}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > maxLaunchLogBytes {
		r.buf = append([]byte{}, r.buf[len(r.buf)-maxLaunchLogBytes:]...)
	}
	return len(p), nil
}

func (r *logRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}

// withLaunchLog appends the tail of the failed process output to a launch error.
func withLaunchLog(source string, recent *logRing, err error) error {
	tail := recent.String()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\n--- %s output ---\n%s", err, filepath.Base(source), tail)
}
