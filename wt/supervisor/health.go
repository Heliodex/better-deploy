package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultHealthPath = "/healthz"
	// defaultHealthInterval is how often a running app's health endpoint is probed unless app.json overrides it.
	defaultHealthInterval = 10 * time.Second
	minHealthInterval     = time.Second
	maxHealthInterval     = time.Hour
	// defaultHealthFailures is the number of consecutive failures before an app is marked unready.
	defaultHealthFailures = 2
	minHealthFailures     = 1
	maxHealthFailures     = 100
	// healthMonitorInterval is the base tick that decides which apps are due for a probe.
	healthMonitorInterval = time.Second
	// healthCheckTimeout caps one probe, so a hung app does not stall the loop.
	healthCheckTimeout = 3 * time.Second
)

var healthClient = &http.Client{
	Timeout: healthCheckTimeout,
	Transport: &http.Transport{
		DialContext:       (&net.Dialer{Timeout: healthCheckTimeout}).DialContext,
		DisableKeepAlives: true,
	},
}

// probeHealth calls an app's health path and requires a 2xx response.
func probeHealth(port int, path string) error {
	resp, err := healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s: %s", resp.Status, trimMessage(body))
	}
	return nil
}

func trimMessage(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// servingState reports whether static files may be served and whether component traffic should be routed.
func (a *app) servingState() (static, ready bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	running := a.cmd != nil && !a.exited && !a.retired
	return running, running && a.ready
}

// healthDue reports whether this app is due for a probe and advances its next probe time.
func (a *app) healthDue(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd == nil || a.exited || a.starting || a.retired {
		return false
	}
	if !a.nextHealth.IsZero() && now.Before(a.nextHealth) {
		return false
	}
	a.nextHealth = now.Add(a.cfg.probeInterval())
	return true
}

// recordHealth folds one probe result into an app's readiness. It reports whether the visible health state changed.
func (a *app) recordHealth(err error, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retired || a.cmd == nil || a.exited {
		return false
	}
	if err == nil {
		changed := !a.healthOK || !a.ready || a.lastError != ""
		a.healthOK = true
		a.ready = true
		a.healthFailures = 0
		a.lastHealthy = now.UTC()
		a.lastError = ""
		return changed
	}

	a.healthFailures++
	a.lastError = err.Error()
	wasReady := a.healthOK && a.ready
	if wasReady && a.healthFailures >= a.cfg.failureLimit() {
		a.healthOK = false
		a.ready = false
		return true
	}
	return a.healthFailures == 1
}

// restartUnhealthy kills a process that failed its health checks when restart_on_unhealthy is set.
// The normal process watcher then applies the usual restart backoff.
func (s *supervisor) restartUnhealthy(a *app) {
	a.mu.Lock()
	should := a.cfg.RestartOnUnhealthy && !a.ready && !a.retired && !a.starting && a.cmd != nil && !a.exited
	if !should || a.unhealthyRestart {
		a.mu.Unlock()
		return
	}
	a.unhealthyRestart = true
	a.mu.Unlock()

	log.Printf("app %s: restarting after failed health checks", a.name)
	go func() {
		defer func() {
			a.mu.Lock()
			a.unhealthyRestart = false
			a.mu.Unlock()
		}()
		s.kill(a)
	}()
}

// checkHealth probes one app and records the result, ignoring probes for apps that are stopped or starting.
func (s *supervisor) checkHealth(a *app) {
	a.mu.Lock()
	running := a.cmd != nil && !a.exited && !a.starting && !a.retired
	port, health := a.port, a.cfg.Health
	a.mu.Unlock()
	if !running {
		return
	}
	err := probeHealth(port, health)
	if a.recordHealth(err, time.Now()) {
		if err != nil {
			log.Printf("app %s: health check failed: %v", a.name, err)
		}
		s.broadcast()
	}
	if err != nil {
		s.restartUnhealthy(a)
	}
}

// checkAllHealth probes apps whose configured interval is due and publishes any status change.
func (s *supervisor) checkAllHealth() {
	now := time.Now()
	var wg sync.WaitGroup
	for _, a := range s.allApps() {
		if !a.healthDue(now) {
			continue
		}
		wg.Add(1)
		go func(a *app) {
			defer wg.Done()
			s.checkHealth(a)
		}(a)
	}
	wg.Wait()
}

// monitorHealth runs periodic health probes until shutdown.
func (s *supervisor) monitorHealth() {
	ticker := time.NewTicker(healthMonitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		if s.stopping.Load() {
			return
		}
		s.checkAllHealth()
	}
}

// statusLocked renders the app's lifecycle and health state. The caller holds a.mu.
func (a *app) statusLocked() string {
	switch {
	case a.retired:
		return "stopped"
	case a.starting || (a.cmd == nil && a.timer != nil):
		return "restarting"
	case a.cmd != nil && !a.exited:
		if a.ready {
			return "running"
		}
		return "unhealthy"
	default:
		return "stopped"
	}
}
