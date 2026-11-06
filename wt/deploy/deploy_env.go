package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"
)

// verifyDefaultTimeout bounds the post-deploy serving probes.
const verifyDefaultTimeout = 90 * time.Second

// verifyPollDelay spaces the post-deploy serving probes. Tests stub it down.
var verifyPollDelay = 2 * time.Second

// envAssignments collects repeatable --env KEY=VALUE flags.
type envAssignments []string

func (e *envAssignments) String() string {
	return strings.Join(*e, ", ")
}

// Set keeps one KEY=VALUE assignment. An empty value clears the key; an empty key is rejected.
func (e *envAssignments) Set(value string) error {
	key, _, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("invalid --env %q: want KEY=VALUE", value)
	}

	*e = append(*e, value)

	return nil
}

// parseDeployEnv folds assignments into a map. Later entries win, so `--env A=1 --env A=2` sends 2.
func parseDeployEnv(assignments []string) (map[string]string, error) {
	vars := make(map[string]string, len(assignments))

	for _, assignment := range assignments {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid --env %q: want KEY=VALUE", assignment)
		}

		vars[strings.TrimSpace(key)] = value
	}

	return vars, nil
}

// applyDeployEnv layers explicit --env values over the manifest. Unlike the dev process-env
// overlay, explicit flags may introduce keys the manifest does not define yet.
func applyDeployEnv(m *manifest, vars map[string]string) {
	if m.Env == nil {
		m.Env = make(map[string]string, len(vars))
	}

	maps.Copy(m.Env, vars)
}

// verifyDeployment polls the supervisor app status and one public HTTP path until the fresh deployment serves.
func verifyDeployment(cfg *deployConfig, m manifest) error {
	if m.Host == "" || strings.Contains(m.Host, "*") || localHost(cfg.host) || localHost(m.Host) {
		fmt.Fprintln(os.Stderr, "verify: skipped (no public app host)")

		return nil
	}

	client := clientOptions{host: cfg.host, token: cfg.token, jsonOut: cfg.jsonOut, timeout: deployPollTimeout}

	return pollVerify(cfg.verifyTimeout, func() (bool, string) {
		return verifyProbe(client, cfg.host, m.Name, m.Host, cfg.verifyPath)
	})
}

// verifyProbe reports whether the app is ready and serving one path. The detail string feeds the final log line either way.
func verifyProbe(client clientOptions, supervisorHost, name, appHost, path string) (bool, string) {
	info, err := routeApp(client, name)
	if err != nil {
		return false, "supervisor: " + err.Error()
	}

	if path == "" {
		path = defaultRoutePath
	}

	report := probeRoute(supervisorHost, appHost, path, deployPollTimeout)

	switch {
	case !report.Routed:
		return false, routeDetail(report)
	case !info.Ready:
		return false, "app status " + info.Status
	case report.HTTPStatus < 200 || report.HTTPStatus >= 400:
		return false, routeDetail(report)
	default:
		return true, fmt.Sprintf("HTTP %d, app %s", report.HTTPStatus, info.Status)
	}
}

// routeDetail compresses a probe report into one line for the verify log.
func routeDetail(report routeReport) string {
	if report.HTTPError != "" {
		return report.HTTPError
	}

	return fmt.Sprintf("HTTP %d", report.HTTPStatus)
}

// pollVerify repeats check until it reports ready or the timeout lapses.
func pollVerify(timeout time.Duration, check func() (bool, string)) error {
	deadline := time.Now().Add(timeout)

	for {
		ready, detail := check()
		if ready {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("verify: not serving after %s (last: %s)", timeout, detail)
		}

		time.Sleep(verifyPollDelay)
	}
}

// deployEnvFlag registers the repeatable --env flag on a flag set.
func deployEnvFlag(flags *flag.FlagSet) *envAssignments {
	vars := envAssignments{}

	flags.Var(&vars, "env", "override one app.json env value (repeatable as KEY=VALUE)")

	return &vars
}
