package app

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := output
	output = buffer
	t.Cleanup(func() { output = previous })
	return buffer
}

func TestLogfPrefixesEveryLevel(t *testing.T) {
	buffer := captureLogs(t)
	for _, level := range []Level{LevelTrace, LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal} {
		Logf(level, "message for %s", level)
	}
	got := buffer.String()
	for _, level := range []Level{LevelTrace, LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal} {
		want := string(level) + " message for " + string(level) + "\n"
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestLogfSplitsLines(t *testing.T) {
	buffer := captureLogs(t)
	Warnf("first\nsecond\n")
	want := "WARN first\nWARN second\n"
	if got := buffer.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriterPrefixesLines(t *testing.T) {
	buffer := captureLogs(t)
	writer := Writer(LevelInfo)
	if _, err := io.WriteString(writer, "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	want := "INFO one\nINFO two\n"
	if got := buffer.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
