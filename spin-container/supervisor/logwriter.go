package main

import (
	"bytes"
	"log"
	"strings"
	"sync"
)

// prefixWriter splits a child process's output into lines and logs each with the process name, so a container with several Spin apps stays readable.
type prefixWriter struct {
	name string
	mu   sync.Mutex
	buf  []byte
}

func newPrefixWriter(name string) *prefixWriter {
	return &prefixWriter{name: name}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			log.Printf("[%s] %s", w.name, line)
		}
	}

	// Avoid unbounded growth if a child writes a long line without a newline.
	if len(w.buf) > 64<<10 {
		log.Printf("[%s] %s", w.name, string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}
