package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	good := `{"schedule":[{"path":"/tick","every":"10s"},{"path":"/tick","every":"1m"},{"name":"prune","path":"/prune","every":"1h","timeout":"10m"}]}`
	if _, err := readAppConfig(writeConfig(t, good), "demo", "localhost", tenantTrusted); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}

	for name, body := range map[string]string{
		"no path":        `{"schedule":[{"every":"10s"}]}`,
		"relative path":  `{"schedule":[{"path":"tick","every":"10s"}]}`,
		"bad interval":   `{"schedule":[{"path":"/tick","every":"often"}]}`,
		"interval floor": `{"schedule":[{"path":"/tick","every":"100ms"}]}`,
		"bad timeout":    `{"schedule":[{"path":"/tick","every":"10s","timeout":"forever"}]}`,
		"duplicate name": `{"schedule":[{"name":"a","path":"/x","every":"10s"},{"name":"a","path":"/y","every":"10s"}]}`,
	} {
		if _, err := readAppConfig(writeConfig(t, body), "demo", "localhost", tenantTrusted); err == nil {
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

		if got := r.Header.Get(scheduleHeader); got != "tick" {
			t.Errorf("%s = %q, want tick", scheduleHeader, got)
		}

		// Every other call fails, so the status records both outcomes.
		if calls.Add(1)%2 == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		}
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
			{Name: "tick", Method: http.MethodPost, Path: "/tick", Every: "1s"},
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

	a.mu.Lock()
	runs := a.scheduleRunsLocked()
	a.mu.Unlock()

	if len(runs) != 1 || runs[0].Name != "tick" || runs[0].LastRun.IsZero() || runs[0].NextRun.IsZero() || runs[0].Every != "1s" {
		t.Errorf("schedule status = %+v", runs)
	}

	// Stopping the app cancels its schedule.
	s.stop(a)

	stoppedAt := calls.Load()

	time.Sleep(1200 * time.Millisecond)

	if got := calls.Load(); got != stoppedAt {
		t.Errorf("schedule kept firing after stop: %d -> %d", stoppedAt, got)
	}
}

func TestScheduleLabels(t *testing.T) {
	got := scheduleLabels([]scheduleConfig{
		{Path: "/tick", Every: "10s"},
		{Path: "/tick", Every: "1m"},
		{Name: "prune", Method: http.MethodGet, Path: "/prune", Every: "1h"},
	})

	if want := []string{"POST /tick", "POST /tick#2", "prune"}; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestRecordScheduleRunCountsFailures(t *testing.T) {
	a := &app{cfg: appConfig{Schedule: []scheduleConfig{{Name: "tick", Path: "/tick", Every: "1s"}}}}

	a.recordScheduleRun(scheduleRun{Name: "tick", Error: "500"}, true)
	a.recordScheduleRun(scheduleRun{Name: "tick", Error: "500"}, true)

	if got := a.scheduleRuns["tick"].Failures; got != 2 {
		t.Errorf("failures = %d, want 2", got)
	}

	a.recordScheduleRun(scheduleRun{Name: "tick", Status: http.StatusOK}, false)

	if got := a.scheduleRuns["tick"].Failures; got != 0 {
		t.Errorf("failures after success = %d, want 0", got)
	}
}
