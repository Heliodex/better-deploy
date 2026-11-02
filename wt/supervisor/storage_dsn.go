package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// storageDSN applies the hardened pragmas to every physical connection through the driver DSN.
// The write pool owns WAL and starts every transaction with BEGIN IMMEDIATE, while both pools get busy_timeout, foreign keys and synchronous=NORMAL.
// A positive quota becomes SQLite's per-database max_page_count, assuming the default 4096-byte page size.
// tempDir sets SQLite's process-wide temp directory; all apps share it.
func storageDSN(path string, readOnly bool, quotaBytes int64, tempDir string) string {
	dsn := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_synchronous", "NORMAL")

	if !readOnly {
		query.Set("_journal_mode", "WAL")
		query.Set("_txlock", "immediate")
	}

	query.Add("_pragma", "trusted_schema(OFF)")
	query.Add("_pragma", "temp_store(FILE)")
	query.Add("_pragma", "cache_size(-2048)")
	query.Add("_pragma", "mmap_size(0)")

	if tempDir != "" {
		// temp_store_directory is deprecated but remains the only in-process way to choose SQLite's temp directory.
		query.Add("_pragma", "temp_store_directory("+sqliteString(tempDir)+")")
	}

	if !readOnly {
		query.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", journalSizeLimit(quotaBytes)))
		query.Add("_pragma", fmt.Sprintf("wal_autocheckpoint(%d)", walAutoCheckpoint(quotaBytes)))
	}

	if quotaBytes > 0 {
		pages := max(quotaBytes/storagePageSize, 1)
		query.Add("_pragma", fmt.Sprintf("max_page_count(%d)", pages))
	}

	if readOnly {
		query.Set("_query_only", "1")
	}

	dsn.RawQuery = query.Encode()

	return dsn.String()
}

// journalSizeLimit scales the WAL truncation limit with the app's database quota, capped at 64 MiB.
func journalSizeLimit(quotaBytes int64) int64 {
	if quotaBytes <= 0 {
		return defaultJournalSizeLimit
	}

	limit := quotaBytes / 8
	if limit < minJournalSizeLimit {
		return minJournalSizeLimit
	}

	if limit > defaultJournalSizeLimit {
		return defaultJournalSizeLimit
	}

	return limit
}

// walAutoCheckpoint keeps the WAL to about a quarter of a small quota, with a 16-page floor and the default 1000-page ceiling.
func walAutoCheckpoint(quotaBytes int64) int {
	if quotaBytes <= 0 {
		return storageWALAutoCheckpoint
	}

	pages := quotaBytes / storagePageSize / 4
	if pages < 16 {
		return 16
	}

	if pages > storageWALAutoCheckpoint {
		return storageWALAutoCheckpoint
	}

	return int(pages)
}

// sqliteString quotes a value for a pragma call in the DSN, doubling embedded single quotes.
func sqliteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// parseStorageMigrationTimeout validates the host-wide deadline for a migration. It defaults to five minutes and stays within one second and one hour.
func parseStorageMigrationTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultStorageMigrationTimeout, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minStorageMigrationTimeout || timeout > maxStorageMigrationTimeout {
		return 0, fmt.Errorf("storage migration timeout %q must be a duration between %s and %s", value, minStorageMigrationTimeout, maxStorageMigrationTimeout)
	}

	return timeout, nil
}

// setQuota records an app's database byte quota and drops its pools when the value changes, so the next request opens them with the new max_page_count.
func (s *storageServer) setQuota(app string, bytes int64) {
	if bytes < 0 {
		bytes = 0
	}

	s.opMu.Lock()
	s.mu.Lock()
	if s.quotas == nil {
		s.quotas = map[string]int64{}
	}

	old, ok := s.quotas[app]
	if ok && old == bytes {
		s.mu.Unlock()
		s.opMu.Unlock()

		return
	}

	s.quotas[app] = bytes
	db := s.dbs[app]
	readDB := s.readDBs[app]
	delete(s.dbs, app)
	delete(s.readDBs, app)
	s.mu.Unlock()
	s.opMu.Unlock()

	if readDB != nil {
		_ = readDB.Close()
	}

	if db != nil {
		_ = db.Close()
	}
}
