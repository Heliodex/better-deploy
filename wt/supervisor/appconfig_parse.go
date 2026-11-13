package main

import (
	"fmt"
	"strings"
	"time"
)

// parseRequestTimeout validates an optional per-request deadline. Empty means no configured deadline;
// "0" and "none" are explicit ways to disable it, which matters where untrusted mode supplies a default.
func parseRequestTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" || strings.EqualFold(value, "none") {
		return 0, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < time.Second || timeout > 24*time.Hour {
		return 0, fmt.Errorf("request_timeout %q must be a duration between 1s and 24h, or 0/none to disable", value)
	}

	return timeout, nil
}

// parseStartupTimeout validates how long a new process may take to answer its health path.
func parseStartupTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return defaultStartupTimeout, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minStartupTimeout || timeout > maxStartupTimeout {
		return 0, fmt.Errorf("startup_timeout %q must be a duration between %s and %s", value, minStartupTimeout, maxStartupTimeout)
	}

	return timeout, nil
}

// parseHealthInterval validates how often a running app is probed.
func parseHealthInterval(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return defaultHealthInterval, nil
	}

	interval, err := time.ParseDuration(value)
	if err != nil || interval < minHealthInterval || interval > maxHealthInterval {
		return 0, fmt.Errorf("health_interval %q must be a duration between %s and %s", value, minHealthInterval, maxHealthInterval)
	}

	return interval, nil
}

// parseHealthFailures validates the consecutive failure count that marks an app unready.
func parseHealthFailures(value int) (int, error) {
	if value == 0 {
		return defaultHealthFailures, nil
	}

	if value < minHealthFailures || value > maxHealthFailures {
		return 0, fmt.Errorf("health_failures must be between %d and %d", minHealthFailures, maxHealthFailures)
	}

	return value, nil
}

// parseIdleTimeout validates how long an app may go without component traffic before its process stops.
// Empty means the default scale-to-zero delay; "0", "none" or "off" disable it.
func parseIdleTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return defaultIdleTimeout, nil
	case value == "0", strings.EqualFold(value, "none"), strings.EqualFold(value, "off"):
		return 0, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minIdleTimeout || timeout > maxIdleTimeout {
		return 0, fmt.Errorf("idle_timeout %q must be a duration between %s and %s, or 0/none/off to disable", value, minIdleTimeout, maxIdleTimeout)
	}

	return timeout, nil
}

func (cfg appConfig) startupLimit() time.Duration {
	if cfg.startupTimeout > 0 {
		return cfg.startupTimeout
	}

	return defaultStartupTimeout
}

func (cfg appConfig) probeInterval() time.Duration {
	if cfg.healthInterval > 0 {
		return cfg.healthInterval
	}

	return defaultHealthInterval
}

func (cfg appConfig) failureLimit() int {
	if cfg.HealthFailures > 0 {
		return cfg.HealthFailures
	}

	return defaultHealthFailures
}

// parseScheduleInterval validates an entry's period and reports how often it fires.
func parseScheduleInterval(entry scheduleConfig) (time.Duration, error) {
	every, err := time.ParseDuration(entry.Every)
	if err != nil {
		return 0, fmt.Errorf("invalid every %q: use a duration such as 10s", entry.Every)
	}

	if every < minScheduleInterval || every > maxScheduleInterval {
		return 0, fmt.Errorf("every %q must be between %s and %s", entry.Every, minScheduleInterval, maxScheduleInterval)
	}

	return every, nil
}

// parseScheduleTimeout reports how long one call of an entry may take. An empty timeout follows the interval, with a floor so short intervals still allow a normal request.
func parseScheduleTimeout(entry scheduleConfig, every time.Duration) (time.Duration, error) {
	if entry.Timeout == "" {
		return max(every, minScheduleTimeout), nil
	}

	timeout, err := time.ParseDuration(entry.Timeout)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: use a duration such as 30s", entry.Timeout)
	}

	if timeout < minScheduleInterval || timeout > maxScheduleInterval {
		return 0, fmt.Errorf("timeout %q must be between %s and %s", entry.Timeout, minScheduleInterval, maxScheduleInterval)
	}

	return timeout, nil
}
