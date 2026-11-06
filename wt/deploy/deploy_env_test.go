package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseDeployEnv(t *testing.T) {
	vars, err := parseDeployEnv([]string{"A=1", "B=hello=world", "EMPTY="})
	if err != nil {
		t.Fatal(err)
	}

	if vars["A"] != "1" || vars["B"] != "hello=world" || vars["EMPTY"] != "" {
		t.Errorf("vars = %v", vars)
	}
}

func TestParseDeployEnvRejectsBareKeys(t *testing.T) {
	for _, assignment := range []string{"NOEQUALS", "=novalue", "   =x"} {
		if _, err := parseDeployEnv([]string{assignment}); err == nil {
			t.Errorf("parseDeployEnv(%q) should fail", assignment)
		}
	}
}

func TestParseDeployEnvLastWins(t *testing.T) {
	vars, err := parseDeployEnv([]string{"A=1", "A=2"})
	if err != nil {
		t.Fatal(err)
	}

	if vars["A"] != "2" {
		t.Errorf("A = %q, want 2", vars["A"])
	}
}

func TestApplyDeployEnv(t *testing.T) {
	m := manifest{}

	applyDeployEnv(&m, map[string]string{"NEW": "1"})
	applyDeployEnv(&m, map[string]string{"NEW": "2", "OTHER": "x"})

	if m.Env["NEW"] != "2" || m.Env["OTHER"] != "x" {
		t.Errorf("env = %v", m.Env)
	}
}

func TestApplyDeployEnvKeepsManifestValues(t *testing.T) {
	m := manifest{Env: map[string]string{"KEEP": "yes"}}

	applyDeployEnv(&m, map[string]string{"NEW": "1"})

	if m.Env["KEEP"] != "yes" || m.Env["NEW"] != "1" {
		t.Errorf("env = %v", m.Env)
	}
}

func TestPollVerifyReadyImmediately(t *testing.T) {
	calls := 0

	err := pollVerify(time.Minute, func() (bool, string) {
		calls++

		return true, "HTTP 200"
	})
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestPollVerifyRetriesUntilReady(t *testing.T) {
	stubPollDelay(t)

	calls := 0

	err := pollVerify(time.Minute, func() (bool, string) {
		calls++

		if calls < 3 {
			return false, "starting"
		}

		return true, "HTTP 200"
	})
	if err != nil {
		t.Fatal(err)
	}

	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestPollVerifyTimesOut(t *testing.T) {
	stubPollDelay(t)

	calls := 0

	err := pollVerify(time.Millisecond, func() (bool, string) {
		calls++

		return false, "still starting"
	})
	if err == nil {
		t.Fatal("pollVerify should time out")
	}

	if !strings.Contains(err.Error(), "still starting") {
		t.Errorf("error = %v, want the last detail", err)
	}

	if calls == 0 {
		t.Error("check should run at least once")
	}
}

// stubPollDelay shrinks the verify sleep so retry tests run in milliseconds.
func stubPollDelay(t *testing.T) {
	t.Helper()

	old := verifyPollDelay
	verifyPollDelay = time.Millisecond

	t.Cleanup(func() { verifyPollDelay = old })
}

func TestRouteDetail(t *testing.T) {
	if got := routeDetail(routeReport{HTTPError: "dial refused"}); got != "dial refused" {
		t.Errorf("detail = %q", got)
	}

	if got := routeDetail(routeReport{HTTPStatus: 503}); got != "HTTP 503" {
		t.Errorf("detail = %q", got)
	}
}

func TestEnvAssignmentsRejectsBareKeys(t *testing.T) {
	var vars envAssignments

	if err := vars.Set("A=1"); err != nil {
		t.Fatal(err)
	}

	if err := vars.Set("BARE"); err == nil {
		t.Error("Set should reject a bare key")
	}

	if len(vars) != 1 {
		t.Errorf("vars = %v", vars)
	}
}

func TestResolveConfigCollectsEnv(t *testing.T) {
	cfg, err := resolveConfig([]string{"--host", "http://127.0.0.1:8080", "--env", "A=1", "--env", "B=2"})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.envVars) != 2 || cfg.envVars[0] != "A=1" || cfg.envVars[1] != "B=2" {
		t.Errorf("envVars = %v", cfg.envVars)
	}

	if cfg.verify || cfg.verifyTimeout != 90*time.Second || cfg.verifyPath != "/" {
		t.Errorf("verify defaults = %v %v %v", cfg.verify, cfg.verifyTimeout, cfg.verifyPath)
	}
}

func TestVerifyDeploymentSkipsWithoutHost(t *testing.T) {
	cfg := &deployConfig{host: "http://127.0.0.1:8080", verifyTimeout: time.Minute}

	for _, m := range []manifest{{}, {Host: "*.example.com"}, {Host: "app.example.com"}} {
		if err := verifyDeployment(cfg, m); err != nil {
			t.Errorf("host %q should skip: %v", m.Host, err)
		}
	}
}
