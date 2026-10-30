package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSupervisor() *supervisor {
	return &supervisor{
		cfg:    config{token: "secret"},
		events: newHub(),
		apps:   map[string]*app{},
		hosts:  map[string]*app{},
	}
}

func TestHandleAppsRequiresAuth(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	rec := httptest.NewRecorder()
	s.handleApps(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.handleApps(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleUnknownApp(t *testing.T) {
	s := testSupervisor()
	for _, target := range []string{"/apps/nope", "/apps/nope/logs", "/apps/nope/resources", "/apps/nope/restart"} {
		method := http.MethodGet
		if strings.HasSuffix(target, "restart") {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestHandleAppMethods(t *testing.T) {
	s := testSupervisor()
	cases := []struct {
		method, target, allow string
	}{
		{http.MethodDelete, "/apps/nope", "GET, POST"},
		{http.MethodPost, "/apps/nope/logs", http.MethodGet},
		{http.MethodPost, "/apps/nope/resources", http.MethodGet},
		{http.MethodGet, "/apps/nope/restart", http.MethodPost},
		{http.MethodPut, "/apps/nope/unknown", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleApp(rec, req)
		if tc.allow == "" {
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.target, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.target, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.target, got, tc.allow)
		}
	}
}

func TestHandleLogsValidation(t *testing.T) {
	s := testSupervisor()
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{name: "test", logs: logs}

	for _, query := range []string{"?tail=0", "?tail=-1", "?tail=abc", "?tail=99999"} {
		req := httptest.NewRequest(http.MethodGet, "/apps/test/logs"+query, nil)
		rec := httptest.NewRecorder()
		s.handleLogs(rec, req, a)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tail %s = %d, want 400", query, rec.Code)
		}
	}
}

func TestAppLogRotation(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 100) + "\n"
	for i := range maxLogBytes/100 + 100 {
		if _, err := fmt.Fprintf(logs, "line %d %s", i, line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(logs.dir, "output.log.1")); err != nil {
		t.Errorf("expected rotated output.log.1: %v", err)
	}

	got, err := logs.tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("tail(5) returned %d lines", len(got))
	}
	for _, entry := range got {
		if !strings.HasPrefix(entry.Message, "line ") {
			t.Errorf("unexpected tail message %q", entry.Message)
		}
		if entry.Time == "" {
			t.Errorf("entry %q has no timestamp", entry.Message)
		}
	}

	got, err = logs.tail(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("tail returned no lines")
	}
}

func TestAppLogStampsCompleteLines(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Output arrives in arbitrary chunks: "hel" + "lo\nwor" + "ld\n".
	for _, chunk := range []string{"hel", "lo\nwor", "ld\n"} {
		if _, err := io.WriteString(logs, chunk); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := logs.tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("tail returned %d entries, want 2: %+v", len(entries), entries)
	}
	for i, want := range []string{"hello", "world"} {
		if entries[i].Message != want {
			t.Errorf("entry %d message = %q, want %q", i, entries[i].Message, want)
		}
		parsed, err := time.Parse(logTimeFormat, entries[i].Time)
		if err != nil {
			t.Errorf("entry %d time %q does not parse: %v", i, entries[i].Time, err)
			continue
		}
		if delta := time.Since(parsed); delta < 0 || delta > time.Minute {
			t.Errorf("entry %d time is not recent: %v", i, delta)
		}
	}
}

func TestParseLogLineToleratesLegacyLines(t *testing.T) {
	legacy := parseLogLine("Serving HTTP on http://127.0.0.1:8080/")
	if legacy.Time != "" || legacy.Message != "Serving HTTP on http://127.0.0.1:8080/" {
		t.Errorf("legacy line parsed as %+v", legacy)
	}
	appStamp := parseLogLine("2026/10/28 14:20:11 app says hi")
	if appStamp.Time != "" || appStamp.Message != "2026/10/28 14:20:11 app says hi" {
		t.Errorf("app-stamped line parsed as %+v", appStamp)
	}
	stamped := parseLogLine("2026-10-28T18:20:11.5Z hello world")
	if stamped.Time != "2026-10-28T18:20:11.5Z" || stamped.Message != "hello world" {
		t.Errorf("stamped line parsed as %+v", stamped)
	}
}

func TestAuthorizedAcceptsCookie(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.AddCookie(&http.Cookie{Name: "wt_token", Value: "secret"})
	if !s.authorized(req) {
		t.Error("cookie token should authorize")
	}
	req = httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.AddCookie(&http.Cookie{Name: "wt_token", Value: "wrong"})
	if s.authorized(req) {
		t.Error("wrong cookie token should not authorize")
	}
}

func TestAdminStaticCacheHeaders(t *testing.T) {
	if _, err := fs.Stat(adminFiles, "web/build/index.html"); err != nil {
		t.Skip("admin panel not built")
	}
	nodes, err := fs.Glob(adminFiles, "web/build/_app/immutable/nodes/*.js")
	if err != nil || len(nodes) == 0 {
		t.Skip("admin panel has no built node bundles")
	}
	for _, tc := range []struct{ path, want string }{
		{"/", "no-cache"},
		{"/_app/immutable/nodes/" + filepath.Base(nodes[0]), "public, max-age=31536000, immutable"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		adminStatic().ServeHTTP(rec, req)
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestHubBroadcast(t *testing.T) {
	h := newHub()
	first := h.subscribe()
	second := h.subscribe()
	h.publish([]byte("hello"))
	for _, ch := range []chan []byte{first, second} {
		select {
		case got := <-ch:
			if string(got) != "hello" {
				t.Errorf("got %q, want hello", got)
			}
		default:
			t.Error("subscriber did not receive broadcast")
		}
	}
	h.unsubscribe(first)
	h.publish([]byte("again"))
	select {
	case <-first:
		t.Error("unsubscribed channel received a broadcast")
	default:
	}
	select {
	case got := <-second:
		if string(got) != "again" {
			t.Errorf("got %q, want again", got)
		}
	default:
		t.Error("remaining subscriber did not receive broadcast")
	}
}

// safeRecorder is a ResponseWriter that can be read while a streaming handler writes.
type safeRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	status int
	wrote  chan struct{}
	once   sync.Once
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{header: http.Header{}, wrote: make(chan struct{})}
}

func (r *safeRecorder) Header() http.Header {
	return r.header
}

func (r *safeRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *safeRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.body.Write(p)
	r.once.Do(func() { close(r.wrote) })
	return n, err
}

func (r *safeRecorder) Flush() {}

func (r *safeRecorder) Body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func TestHandleEventsStreamsSnapshot(t *testing.T) {
	s := testSupervisor()
	s.apps["demo"] = &app{name: "demo", cfg: appConfig{Host: "demo.example"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	rec := newSafeRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.handleEvents(rec, req)
	}()
	select {
	case <-rec.wrote:
	case <-time.After(5 * time.Second):
		cancel()
		<-finished
		t.Fatal("timed out waiting for snapshot")
	}
	cancel()
	<-finished
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if body := rec.Body(); !strings.Contains(body, `"name":"demo"`) {
		t.Errorf("snapshot missing app: %s", body)
	}
}

func TestHandleEventsRequiresAuth(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	s.handleEvents(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleLogStreamBacklogAndLines(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(logs, "existing line\n"); err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/logs/stream?tail=10", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	rec := newSafeRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.handleLogStream(rec, req, &app{name: "demo", logs: logs})
	}()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for !strings.Contains(rec.Body(), want) {
			select {
			case <-deadline:
				cancel()
				<-finished
				t.Fatalf("timed out waiting for %q in %s", want, rec.Body())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	waitFor(`"type":"backlog"`)
	if body := rec.Body(); !strings.Contains(body, `"message":"existing line"`) {
		t.Errorf("backlog missing existing line: %s", body)
	}

	if _, err := io.WriteString(logs, "fresh line\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(`"type":"line"`)
	waitFor(`"message":"fresh line"`)

	cancel()
	<-finished
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}

func TestHandleLogStreamRequiresAuth(t *testing.T) {
	s := testSupervisor()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/logs/stream", nil)
	rec := httptest.NewRecorder()
	s.handleLogStream(rec, req, &app{name: "demo"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAppLogStripsGuestPrefix(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(logs, "stderr [2] :: hello\n"); err != nil {
		t.Fatal(err)
	}
	entries, err := logs.tail(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "hello" {
		t.Errorf("entries = %+v, want one with message hello", entries)
	}
}

func TestSplitLevelPrefix(t *testing.T) {
	cases := []struct{ line, level, message string }{
		{"WARN storage slow", "WARN", "storage slow"},
		{"error boom", "ERROR", "boom"},
		{"TRACE deep", "TRACE", "deep"},
		{"FATAL done", "FATAL", "done"},
		{"plain message", "INFO", "plain message"},
		{"WARNING not a level", "INFO", "WARNING not a level"},
		{"INFO", "INFO", "INFO"},
	}
	for _, tc := range cases {
		level, message := splitLevelPrefix(tc.line)
		if level != tc.level || message != tc.message {
			t.Errorf("splitLevelPrefix(%q) = %q, %q; want %q, %q", tc.line, level, message, tc.level, tc.message)
		}
	}
}

func TestAppLogRecordsLevels(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(logs, "WARN careful\nplain line\n"); err != nil {
		t.Fatal(err)
	}
	entries, err := logs.tail(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []logEntry{
		{Time: entries[0].Time, Level: "warn", Message: "careful"},
		{Time: entries[1].Time, Level: "info", Message: "plain line"},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	for i := range want {
		if entries[i].Level != want[i].Level || entries[i].Message != want[i].Message {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestAppLogSeparatesSupervisorLines(t *testing.T) {
	logs, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(logs, "WARN careful\n"); err != nil {
		t.Fatal(err)
	}
	logs.logSupervisor("GET 200 1.5ms /hello")
	if _, err := io.WriteString(logs, "plain line\n"); err != nil {
		t.Fatal(err)
	}
	entries, err := logs.tail(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []logEntry{
		{Level: "warn", Source: "app", Message: "careful"},
		{Level: "info", Source: "supervisor", Message: "GET 200 1.5ms /hello"},
		{Level: "info", Source: "app", Message: "plain line"},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	for i := range want {
		if entries[i].Level != want[i].Level || entries[i].Source != want[i].Source || entries[i].Message != want[i].Message {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
		if entries[i].Time == "" {
			t.Errorf("entry %d has no timestamp", i)
		}
	}
}

func TestParseLogLineReadsLegacyLinesAsInfo(t *testing.T) {
	legacy := parseLogLine("2026-10-28T21:00:00Z Serving HTTP on http://127.0.0.1:8080/")
	if legacy.Level != "info" || legacy.Source != "app" || legacy.Message != "Serving HTTP on http://127.0.0.1:8080/" {
		t.Errorf("legacy line parsed as %+v", legacy)
	}
	stamped := parseLogLine("2026-10-28T21:00:00Z ERROR broke")
	if stamped.Level != "error" || stamped.Source != "app" || stamped.Message != "broke" {
		t.Errorf("leveled line parsed as %+v", stamped)
	}
}
