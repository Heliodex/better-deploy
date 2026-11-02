package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogFilterAndExport(t *testing.T) {
	log, err := newAppLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()

	if _, err := log.Write([]byte("INFO hello world\nWARN danger zone\n")); err != nil {
		t.Fatal(err)
	}

	filtered, err := log.tailFiltered(10, logFilter{level: "warn"})
	if err != nil {
		t.Fatal(err)
	}

	if len(filtered) != 1 || !strings.Contains(filtered[0].Message, "danger") {
		t.Errorf("filtered = %+v", filtered)
	}

	s := testSupervisor()
	a := &app{name: "demo", logs: log}
	s.apps["demo"] = a
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/logs?level=warn&q=anger", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "danger zone") {
		t.Fatalf("filtered logs = %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "hello world") {
		t.Error("filtered logs include the other line")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/apps/demo/logs/export?format=jsonl&level=info", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `"level":"info"`) || !strings.Contains(rec.Body.String(), "hello world") {
		t.Errorf("export body = %q", rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "danger zone") {
		t.Error("export included a filtered line")
	}

	if !strings.Contains(rec.Header().Get("Content-Disposition"), "demo.log.jsonl") {
		t.Errorf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
	}
}
