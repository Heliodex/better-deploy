package app

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Level is the severity prefix the supervisor parses out of an app's log lines.
type Level string

const (
	LevelTrace Level = "TRACE"
	LevelDebug Level = "DEBUG"
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
	LevelFatal Level = "FATAL"
)

// output is swapped in tests; production writes to stderr, which wasmtime forwards.
var output io.Writer = os.Stderr

// Logf writes one line per message line as "<LEVEL> <message>". The supervisor stamps it with the receive time and strips the level, so the app adds neither a timestamp nor a prefix of its own.
func Logf(level Level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	var builder strings.Builder
	for line := range strings.Lines(message) {
		builder.WriteString(string(level))
		builder.WriteByte(' ')
		builder.WriteString(strings.TrimSuffix(line, "\n"))
		builder.WriteByte('\n')
	}
	_, _ = io.WriteString(output, builder.String())
}

func Tracef(format string, args ...any) { Logf(LevelTrace, format, args...) }
func Debugf(format string, args ...any) { Logf(LevelDebug, format, args...) }
func Infof(format string, args ...any)  { Logf(LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { Logf(LevelWarn, format, args...) }
func Errorf(format string, args ...any) { Logf(LevelError, format, args...) }

// Fatalf logs at FATAL and exits the component. A component instance should not normally exit mid-request, so prefer Errorf unless the process really is done.
func Fatalf(format string, args ...any) {
	Logf(LevelFatal, format, args...)
	os.Exit(1)
}

// Writer prefixes every complete line written to it, for handing to the stdlib logger: log.SetFlags(0); log.SetOutput(app.Writer(app.LevelInfo)).
func Writer(level Level) io.Writer {
	return levelWriter{level: level}
}

type levelWriter struct {
	level Level
}

func (w levelWriter) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		if line == "\n" {
			continue
		}
		Logf(w.level, "%s", strings.TrimSuffix(line, "\n"))
	}
	return len(p), nil
}
