package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type logsResponse struct {
	Logs []logEntry `json:"logs"`
}

// logTailParam reads the ?tail= query, writing 400 when it is out of range.
func logTailParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	value := r.URL.Query().Get("tail")
	if value == "" {
		return defaultLogTail, true
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maxLogTail {
		writeError(w, http.StatusBadRequest, "tail must be between 1 and 5000")
		return 0, false
	}

	return parsed, true
}

// logFilterParam parses level, source, text and time filters and writes 400 on invalid input.
func logFilterParam(w http.ResponseWriter, r *http.Request) (logFilter, bool) {
	filter := logFilter{
		level:  strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level"))),
		source: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source"))),
		query:  strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))),
	}
	if filter.source != "" && filter.source != "app" && filter.source != "supervisor" {
		writeError(w, http.StatusBadRequest, "source must be app or supervisor")
		return filter, false
	}

	if filter.level != "" {
		switch filter.level {
		case "trace", "debug", "info", "warn", "error", "fatal":
		default:
			writeError(w, http.StatusBadRequest, "unknown log level")
			return filter, false
		}
	}

	for _, value := range []struct {
		name string
		dest *time.Time
	}{{"since", &filter.since}, {"until", &filter.until}} {
		if raw := r.URL.Query().Get(value.name); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, value.name+" must be RFC3339")
				return filter, false
			}

			*value.dest = parsed
		}
	}

	return filter, true
}

func (s *supervisor) handleLogs(w http.ResponseWriter, r *http.Request, a *app) {
	tail, ok := logTailParam(w, r)
	if !ok {
		return
	}

	filter, ok := logFilterParam(w, r)
	if !ok {
		return
	}

	logs := []logEntry{}
	if a.logs != nil {
		var err error

		logs, err = a.logs.tailFiltered(tail, filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, logsResponse{Logs: logs})
}

// handleLogExport downloads every retained matching line as text or JSON lines.
func (s *supervisor) handleLogExport(w http.ResponseWriter, r *http.Request, a *app) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	filter, ok := logFilterParam(w, r)
	if !ok {
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "text"
	}

	if format != "text" && format != "jsonl" {
		writeError(w, http.StatusBadRequest, "format must be text or jsonl")
		return
	}

	entries := []logEntry{}

	if a.logs != nil {
		var err error

		entries, err = a.logs.allFiltered(filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	w.Header().Set("Cache-Control", "no-store")

	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+a.name+`.log.jsonl"`)

		for _, entry := range entries {
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}

			_, _ = w.Write(append(data, '\n')) //nolint:gosec // The body is escaped or JSON-encoded before writing.
		}

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+a.name+`.log"`)

	for _, entry := range entries {
		_, _ = fmt.Fprintf(w, "%s %s %s %s\n", entry.Time, entry.Level, entry.Source, entry.Message)
	}
}

// logStreamMessage is one SSE frame: the backlog on connect, then single lines.
type logStreamMessage struct {
	Type string     `json:"type"`
	Logs []logEntry `json:"logs,omitempty"`
	Log  *logEntry  `json:"log,omitempty"`
}

// handleLogStream streams an app's log: the recent backlog on connect, then each new line as the supervisor writes it, with comment pings to hold the connection.
func (s *supervisor) handleLogStream(w http.ResponseWriter, r *http.Request, a *app) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	if !s.requireAuth(w, r) {
		return
	}

	tail, ok := logTailParam(w, r)
	if !ok {
		return
	}

	stream, ok := newSSEStream(w, "no-cache")
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	send := func(message logStreamMessage) bool {
		data, err := json.Marshal(message)
		if err != nil {
			return false
		}

		return stream.data(data)
	}

	filter, ok := logFilterParam(w, r)
	if !ok {
		return
	}

	backlog := []logEntry{}

	var messages chan logEntry

	if a.logs != nil {
		var err error

		backlog, err = a.logs.tailFiltered(tail, filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		messages = a.logs.subscribe()
		defer a.logs.unsubscribe(messages)
	}

	if !send(logStreamMessage{Type: "backlog", Logs: backlog}) {
		return
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case entry := <-messages:
			if !filter.matches(entry) {
				continue
			}

			if !send(logStreamMessage{Type: "line", Log: &entry}) {
				return
			}
		case <-ticker.C:
			if !stream.ping() {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
