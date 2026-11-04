// Idle scale-to-zero: apps sleep after an idle timeout, default ten minutes, and start again on the next request.

package main

import (
	"errors"
	"log"
	"time"
)

// idleMonitorInterval is how often the supervisor checks whether an app has been idle long enough to stop.
const idleMonitorInterval = time.Second

// idleState reports whether the app was stopped by the idle monitor rather than an operator.
func (a *app) idleState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.idleStopped
}

// idleDue reports whether a running app has been idle long enough. The caller must not hold a.mu.
func (a *app) idleDue(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cfg.idleTimeout <= 0 || a.idleStopped || a.retired || a.starting || a.draining {
		return false
	}

	if len(a.cfg.Schedule) > 0 || a.cmd == nil || a.exited || a.inflight > 0 {
		return false
	}

	last := a.lastRequest
	if last.IsZero() {
		last = a.started
	}

	return !last.IsZero() && now.Sub(last) >= a.cfg.idleTimeout
}

// markIdleStopped rechecks the idle state under the app lock and marks the app as idle-stopped so a concurrent request knows to start it.
func (a *app) markIdleStopped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cfg.idleTimeout <= 0 || a.idleStopped || a.retired || a.starting || a.draining {
		return false
	}

	if len(a.cfg.Schedule) > 0 || a.cmd == nil || a.exited || a.inflight > 0 {
		return false
	}

	a.idleStopped = true

	return true
}

// wakeIdleApp starts an app that the idle monitor stopped. It is safe to call for a running app.
func (s *supervisor) wakeIdleApp(a *app) error {
	a.mu.Lock()
	idle := a.idleStopped
	running := a.cmd != nil && !a.exited
	a.mu.Unlock()

	if !idle || running {
		return nil
	}

	if s.lookup(a.name) != a {
		return errors.New("app is no longer registered")
	}

	log.Printf("app %s: waking after idle", a.name)

	return s.startStopped(a)
}

// wakeIdleAppAsync starts an app in the background, coalescing concurrent wake attempts behind startMu.
func (s *supervisor) wakeIdleAppAsync(a *app) {
	go func() {
		if err := s.wakeIdleApp(a); err != nil {
			log.Printf("app %s: wake after idle: %v", a.name, err)
		}
	}()
}

// monitorIdle stops apps that have gone too long without client traffic.
func (s *supervisor) monitorIdle() {
	ticker := time.NewTicker(idleMonitorInterval)
	defer ticker.Stop()

	for range ticker.C {
		if s.stopping.Load() {
			return
		}

		s.stopIdleApps(time.Now())
	}
}

// stopIdleApps checks every app once and stops those whose idle timeout has elapsed and that have no queued job work.
func (s *supervisor) stopIdleApps(now time.Time) {
	for _, a := range s.allApps() {
		if !a.idleDue(now) {
			continue
		}

		if s.jobs != nil && s.jobs.busy(a.name) {
			continue
		}

		if !a.markIdleStopped() {
			continue
		}

		log.Printf("app %s: stopping after idle", a.name)
		s.drainAndStop(a, "idle")
	}
}
