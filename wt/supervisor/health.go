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
	// healthCheckInterval is how often a running app's health endpoint is probed.
	healthCheckInterval = 10 * time.Second
	// healthCheckTimeout caps one probe, so a hung app does not stall the loop.
	healthCheckTimeout = 3 * time.Second
	// healthFailureThreshold is the number of consecutive failures before a running app is reported unhealthy.
	healthFailureThreshold = 2
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

// recordHealth folds one probe result into an app's status. It reports whether the visible health state changed.
func (a *app) recordHealth(err error, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retired || a.cmd == nil || a.exited {
		return false
	}
	if err == nil {
		changed := !a.healthOK || a.lastError != ""
		a.healthOK = true
		a.healthFailures = 0
		a.lastHealthy = now.UTC()
		a.lastError = ""
		return changed
	}

	a.healthFailures++
	a.lastError = err.Error()
	if a.healthOK && a.healthFailures >= healthFailureThreshold {
		a.healthOK = false
		return true
	}
	return a.healthFailures == 1
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
}

// checkAllHealth probes every registered app concurrently and publishes any status change.
func (s *supervisor) checkAllHealth() {
	var wg sync.WaitGroup
	for _, a := range s.allApps() {
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
	ticker := time.NewTicker(healthCheckInterval)
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
		if a.healthOK {
			return "running"
		}
		return "unhealthy"
	default:
		return "stopped"
	}
}
