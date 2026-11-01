package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMetricsEndpointRendersCountersAndGauges(t *testing.T) {
	s := testSupervisor()
	s.metrics = newMetrics()
	a := &app{name: "demo", cfg: appConfig{Host: "demo.localhost"}, cmd: &exec.Cmd{}, ready: true}
	s.apps["demo"] = a
	s.metrics.beginRequest("demo")
	s.metrics.endRequest("demo", http.MethodGet, http.StatusOK, 50*time.Millisecond)
	s.metrics.observeStorage("demo", "read", nil, 2*time.Millisecond)
	s.metrics.observeStorage("demo", "write", errors.New("busy"), 3*time.Millisecond)
	s.metrics.observeDeploy("demo", "success")
	s.metrics.observeBackup("demo", "create", "success")
	s.metrics.observeJob("demo", "succeeded")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`wt_requests_total{app="demo",method="GET",status="200"} 1`,
		`wt_request_duration_seconds_count{app="demo",method="GET"} 1`,
		`wt_inflight_requests{app="demo"} 0`,
		`wt_storage_operations_total{app="demo",pool="read",result="ok"} 1`,
		`wt_storage_operations_total{app="demo",pool="write",result="error"} 1`,
		`wt_deploy_total{app="demo",action="deploy",result="success"} 1`,
		`wt_backup_total{app="demo",action="create",result="success"} 1`,
		`wt_jobs_total{app="demo",action="job",result="succeeded"} 1`,
		`wt_app_ready{app="demo"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q\n%s", want, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated metrics = %d, want 401", rec.Code)
	}
}
