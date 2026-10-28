package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseScheduleInterval(t *testing.T) {
	valid := map[string]time.Duration{
		"1s":  time.Second,
		"10s": 10 * time.Second,
		"1m":  time.Minute,
		"24h": maxScheduleInterval,
	}
	for every, want := range valid {
		got, err := parseScheduleInterval(scheduleConfig{Path: "/tick", Every: every})
		if err != nil || got != want {
			t.Errorf("%q => %v, %v; want %v", every, got, err, want)
		}
	}
	for _, every := range []string{"", "0s", "500ms", "25h", "soon"} {
		if _, err := parseScheduleInterval(scheduleConfig{Path: "/tick", Every: every}); err == nil {
			t.Errorf("%q should be rejected", every)
		}
	}
}

func TestReadAppConfigValidatesSchedule(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	good := `{"schedule":[{"path":"/tick","every":"10s"}]}`
	if _, err := readAppConfig(writeConfig(t, good), "demo", "localhost"); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}

	for name, body := range map[string]string{
		"no path":        `{"schedule":[{"every":"10s"}]}`,
		"relative path":  `{"schedule":[{"path":"tick","every":"10s"}]}`,
		"bad interval":   `{"schedule":[{"path":"/tick","every":"often"}]}`,
		"interval floor": `{"schedule":[{"path":"/tick","every":"100ms"}]}`,
	} {
		if _, err := readAppConfig(writeConfig(t, body), "demo", "localhost"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestScheduleCallsEndpoint(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		calls.Add(1)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := testSupervisor()
	a := &app{
		name: "demo",
		port: port,
		cfg: appConfig{Schedule: []scheduleConfig{
			{Method: http.MethodPost, Path: "/tick", Every: "1s"},
		}},
	}
	s.startSchedule(a)
	defer s.stop(a)

	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("endpoint called %d times, want at least 2", got)
	}

	// Stopping the app cancels its schedule.
	s.stop(a)
	stoppedAt := calls.Load()
	time.Sleep(1200 * time.Millisecond)
	if got := calls.Load(); got != stoppedAt {
		t.Errorf("schedule kept firing after stop: %d -> %d", stoppedAt, got)
	}
}
