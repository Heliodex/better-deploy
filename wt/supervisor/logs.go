package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultLogTail = 200
	maxLogTail     = 5000
	maxLogBytes    = 1 << 20
	logTimeFormat  = time.RFC3339Nano
)

// appLog appends an app's output to capped log files for the admin panel. Files live outside the app's /data preopen so components cannot read them. Each line is stamped with the time the supervisor received it, so timestamps do not depend on how an app logs. The app's own output goes to output.log; supervisor-side lines for the app, such as proxied requests, go to supervisor.log so the two read apart.
type appLog struct {
	mu             sync.Mutex
	dir            string
	file           *os.File
	size           int64
	supervisorFile *os.File
	supervisorSize int64
	pending        []byte
	subs           map[chan logEntry]struct{}
	secrets        []string
	closed         bool
}

// setSecrets replaces the values redacted from app and supervisor log lines. Longer values sort first so one secret cannot mask part of another.
func (l *appLog) setSecrets(values []string) {
	unique := make([]string, 0, len(values))
	seen := map[string]struct{}{}

	for _, value := range values {
		if value == "" {
			continue
		}

		if _, ok := seen[value]; ok {
			continue
		}

		seen[value] = struct{}{}
		unique = append(unique, value)
	}

	sort.Slice(unique, func(i, j int) bool {
		if len(unique[i]) != len(unique[j]) {
			return len(unique[i]) > len(unique[j])
		}

		return unique[i] < unique[j]
	})
	l.mu.Lock()
	l.secrets = unique
	l.mu.Unlock()
}

// redactLocked replaces secret values in a line. Callers hold l.mu.
func (l *appLog) redactLocked(text string) string {
	return redactSecrets(text, l.secrets)
}

// subscribe registers a buffered channel for live log lines.
func (l *appLog) subscribe() chan logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	messages := make(chan logEntry, 128)
	l.subs[messages] = struct{}{}

	return messages
}

// unsubscribe removes a channel without closing it, so a concurrent publish cannot panic.
func (l *appLog) unsubscribe(messages chan logEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.subs, messages)
}

// publish fans a line out to streaming clients. Callers hold l.mu, and delivery is non-blocking so a stalled client can never hold up the app's output.
func (l *appLog) publish(entry logEntry) {
	for messages := range l.subs {
		select {
		case messages <- entry:
		default:
		}
	}
}

func (l *appLog) path() string {
	return filepath.Join(l.dir, "output.log")
}

func (l *appLog) backupPath() string {
	return filepath.Join(l.dir, "output.log.1")
}

func (l *appLog) supervisorPath() string {
	return filepath.Join(l.dir, "supervisor.log")
}

func (l *appLog) supervisorBackupPath() string {
	return filepath.Join(l.dir, "supervisor.log.1")
}

// openLogFile opens an append-only log file and returns its current size.
func openLogFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, 0, err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}

	return file, info.Size(), nil
}

// rotateLogFile closes the current file, moves it to backup and opens a fresh one.
func rotateLogFile(file *os.File, path, backup string) (*os.File, int64, error) {
	if err := file.Close(); err != nil {
		return nil, 0, err
	}

	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err != nil && !os.IsNotExist(err) {
		return nil, 0, err
	}

	return openLogFile(path)
}

func (l *appLog) open() error {
	file, size, err := openLogFile(l.path())
	if err != nil {
		return err
	}

	l.file, l.size = file, size

	supervisorFile, supervisorSize, err := openLogFile(l.supervisorPath())
	if err != nil {
		_ = file.Close()
		return err
	}

	l.supervisorFile, l.supervisorSize = supervisorFile, supervisorSize

	return nil
}

// newAppLog opens an app's output and supervisor logs.
func newAppLog(dir string) (*appLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	out := &appLog{dir: dir, subs: map[chan logEntry]struct{}{}}
	if err := out.open(); err != nil {
		return nil, err
	}

	return out, nil
}

// close stops writes and releases the app's log files. It is used when an app is deleted.
func (l *appLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return
	}

	l.closed = true
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}

	if l.supervisorFile != nil {
		_ = l.supervisorFile.Close()
		l.supervisorFile = nil
	}
}

// logSupervisor records a supervisor-side line for the app, such as a proxied request.
// It streams and persists alongside the app's own output, marked so the two read apart.
func (l *appLog) logSupervisor(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return
	}

	message = l.redactLocked(message)
	stamp := time.Now().UTC().Format(logTimeFormat)

	data := []byte(stamp + " INFO " + message + "\n")
	if l.supervisorSize+int64(len(data)) > maxLogBytes {
		file, size, err := rotateLogFile(l.supervisorFile, l.supervisorPath(), l.supervisorBackupPath())
		if err != nil {
			return
		}

		l.supervisorFile, l.supervisorSize = file, size
	}

	written, err := l.supervisorFile.Write(data)
	l.supervisorSize += int64(written)

	if err != nil {
		return
	}

	l.publish(logEntry{Time: stamp, Level: "info", Source: "supervisor", Message: message})
}

// guestStderr matches wasmtime's prefix for a guest's own stderr/stdout, e.g. "stderr [1] :: ".
var guestStderr = regexp.MustCompile(`^(?:stderr|stdout) \[\d+\] :: `)

// splitLevelPrefix pulls an app's "<LEVEL> " prefix off a line. Anything unrecognized is part of the message and the level is info.
func splitLevelPrefix(line string) (string, string) {
	token, rest, ok := strings.Cut(line, " ")
	if !ok {
		return "INFO", line
	}

	upper := strings.ToUpper(token)
	for _, level := range logLevels {
		if upper == level {
			return level, rest
		}
	}

	return "INFO", line
}

// writeLine stamps one complete line of the app's own output and appends it, rotating past maxLogBytes.
func (l *appLog) writeLine(line string) error {
	if l.closed {
		return nil
	}

	line = guestStderr.ReplaceAllString(line, "")
	level, message := splitLevelPrefix(line)
	message = l.redactLocked(message)
	stamp := time.Now().UTC().Format(logTimeFormat)

	data := []byte(stamp + " " + level + " " + message + "\n")
	if l.size+int64(len(data)) > maxLogBytes {
		file, size, err := rotateLogFile(l.file, l.path(), l.backupPath())
		if err != nil {
			return err
		}

		l.file, l.size = file, size
	}

	written, err := l.file.Write(data)
	l.size += int64(written)

	if err != nil {
		return err
	}

	l.publish(logEntry{Time: stamp, Level: strings.ToLower(level), Source: "app", Message: message})

	return nil
}

// logLevels are the prefixes an app may put at the start of a line, most verbose first.
var logLevels = []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"}

// logEntry is one log line with the time the supervisor received it, its level, and which side wrote it: the app process or the supervisor. Lines written before sources existed have no marker and read as app output.
type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

// Write implements io.Writer. Output arrives in arbitrary chunks, so lines are buffered until their newline and each one is written with a receive timestamp.
func (l *appLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return len(p), nil
	}

	l.pending = append(l.pending, p...)
	for {
		index := bytes.IndexByte(l.pending, '\n')
		if index < 0 {
			break
		}

		line := string(l.pending[:index])

		l.pending = l.pending[index+1:]
		if err := l.writeLine(line); err != nil {
			return 0, err
		}
	}
	// A writer that never emits a newline must not grow the buffer without bound.
	if len(l.pending) > maxLogBytes {
		l.pending = append([]byte{}, l.pending[len(l.pending)-maxLogBytes:]...)
	}

	return len(p), nil
}

// parseSourcedLogLine parses a line from one of the app's log files, marking which side wrote it.
func parseSourcedLogLine(line, source string) logEntry {
	stamp, rest, ok := strings.Cut(line, " ")
	if !ok {
		return logEntry{Level: "info", Source: source, Message: line}
	}

	if _, err := time.Parse(logTimeFormat, stamp); err != nil {
		return logEntry{Level: "info", Source: source, Message: line}
	}

	level, message := splitLevelPrefix(rest)

	return logEntry{Time: stamp, Level: strings.ToLower(level), Source: source, Message: message}
}

// parseLogLine splits a stamped line into its time and message.
func parseLogLine(line string) logEntry {
	return parseSourcedLogLine(line, "app")
}

// logFilter narrows a log tail or export by level, source, text and time.
type logFilter struct {
	level, source, query string
	since, until         time.Time
}

func (f logFilter) matches(entry logEntry) bool {
	if f.level != "" && entry.Level != f.level {
		return false
	}

	if f.source != "" && entry.Source != f.source {
		return false
	}

	if f.query != "" && !strings.Contains(strings.ToLower(entry.Message), f.query) {
		return false
	}

	if !f.since.IsZero() || !f.until.IsZero() {
		stamp, err := time.Parse(time.RFC3339Nano, entry.Time)
		if err != nil {
			return false
		}

		if !f.since.IsZero() && stamp.Before(f.since) {
			return false
		}

		if !f.until.IsZero() && stamp.After(f.until) {
			return false
		}
	}

	return true
}

// readLocked reads every retained line from both sources, oldest first.
func (l *appLog) readLocked() ([]logEntry, error) {
	files := []struct {
		backup, path, source string
	}{
		{l.backupPath(), l.path(), "app"},
		{l.supervisorBackupPath(), l.supervisorPath(), "supervisor"},
	}
	out := []logEntry{}

	for _, file := range files {
		for _, path := range []string{file.backup, file.path} {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}

			if err != nil {
				return nil, err
			}

			for line := range strings.SplitSeq(string(data), "\n") {
				if line == "" {
					continue
				}

				entry := parseSourcedLogLine(line, file.source)
				entry.Message = l.redactLocked(entry.Message)
				out = append(out, entry)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })

	return out, nil
}

// tailFiltered returns the last n matching lines from both the app and the supervisor, oldest first.
func (l *appLog) tailFiltered(n int, filter logFilter) ([]logEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	all, err := l.readLocked()
	if err != nil {
		return nil, err
	}

	out := make([]logEntry, 0, min(n, len(all)))
	for _, entry := range all {
		if filter.matches(entry) {
			out = append(out, entry)
		}
	}

	if len(out) > n {
		out = out[len(out)-n:]
	}

	return out, nil
}

// tail returns the last n log lines from both the app and the supervisor, oldest first.
func (l *appLog) tail(n int) ([]logEntry, error) {
	return l.tailFiltered(n, logFilter{})
}

// allFiltered returns every retained matching line, for export. The retained files are capped, so this is bounded too.
func (l *appLog) allFiltered(filter logFilter) ([]logEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	all, err := l.readLocked()
	if err != nil {
		return nil, err
	}

	out := make([]logEntry, 0, len(all))
	for _, entry := range all {
		if filter.matches(entry) {
			out = append(out, entry)
		}
	}

	return out, nil
}
