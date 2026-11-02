package main

import (
	"fmt"
	"net/http"
)

// sseStream writes Server-Sent Events with a flushed event-stream header.
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEStream sets the common headers, writes 200 and flushes them.
func newSSEStream(w http.ResponseWriter, cacheControl string) (*sseStream, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseStream{w: w, flusher: flusher}, true
}

// write formats and flushes one frame, reporting whether the client is still connected.
func (s *sseStream) write(format string, args ...any) bool {
	if _, err := fmt.Fprintf(s.w, format, args...); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

// data writes one unnamed event frame.
func (s *sseStream) data(payload []byte) bool {
	return s.write("data: %s\n\n", payload)
}

// event writes one named event frame.
func (s *sseStream) event(name string, payload []byte) bool {
	return s.write("event: %s\ndata: %s\n\n", name, payload)
}

// idEvent writes one named event with a resumable id.
func (s *sseStream) idEvent(id int64, name string, payload []byte) bool {
	return s.write("id: %d\nevent: %s\ndata: %s\n\n", id, name, payload)
}

// comment writes a comment frame, used as a keep-alive ping.
func (s *sseStream) comment(text string) bool {
	return s.write(": %s\n\n", text)
}

// retry tells the browser how long to wait before reconnecting.
func (s *sseStream) retry(milliseconds int) bool {
	return s.write("retry: %d\n\n", milliseconds)
}

// ping sends a keep-alive comment frame.
func (s *sseStream) ping() bool {
	return s.comment("ping")
}
