package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

// envBool reads a boolean opt-in/opt-out. An unknown value keeps the fallback, so a typo cannot silently disable a boundary.
func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return fallback
	}
}

// envInt64 reads a positive integer from the environment, keeping the fallback for invalid values.
func envInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

// envStorageMigrationTimeout applies WT_STORAGE_MIGRATION_TIMEOUT, falling back to the default for an empty or invalid value. The explicit flag is validated separately.
func envStorageMigrationTimeout() time.Duration {
	timeout, err := parseStorageMigrationTimeout(os.Getenv("WT_STORAGE_MIGRATION_TIMEOUT"))
	if err != nil {
		return defaultStorageMigrationTimeout
	}

	return timeout
}
