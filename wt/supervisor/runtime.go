// Process and environment helpers shared by launching and deploying apps.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	// serveAddr lets wasmtime bind any free loopback port; the supervisor learns the chosen port from its startup line.
	serveAddr          = "127.0.0.1:0"
	serveAddressPrefix = "Serving HTTP on http://127.0.0.1:"
	maxPortLineBytes   = 8 << 10
)

// portFinder watches a wasmtime process's output for the actual address it bound.
type portFinder struct {
	mu      sync.Mutex
	pending []byte
	port    chan int
	once    sync.Once
}

func newPortFinder() *portFinder {
	return &portFinder{port: make(chan int, 1)}
}

func (p *portFinder) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, data...)
	for {
		index := bytes.IndexByte(p.pending, '\n')
		if index < 0 {
			break
		}
		line := string(p.pending[:index])
		p.pending = p.pending[index+1:]
		if port, ok := parseServePort(line); ok {
			p.once.Do(func() { p.port <- port })
		}
	}
	if len(p.pending) > maxPortLineBytes {
		p.pending = append([]byte{}, p.pending[len(p.pending)-maxPortLineBytes:]...)
	}
	return len(data), nil
}

func parseServePort(line string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), serveAddressPrefix)
	if !ok {
		return 0, false
	}
	portText, _, _ := strings.Cut(rest, "/")
	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
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
