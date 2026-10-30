package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	storageMaintenanceTimeout = 60 * time.Second
	storageMaintenanceOps     = "vacuum, optimize, checkpoint, integrity_check"
)

// maintenance runs one administrative SQLite operation that raw app SQL cannot request.
func (s *storageServer) maintenance(ctx context.Context, app, op string) (string, error) {
	switch op {
	case "vacuum":
		err := s.withConn(ctx, app, func(conn *sql.Conn) error {
			// VACUUM uses a temporary database internally, which the per-query attachment limit would otherwise block.
			if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, 10); err != nil {
				return err
			}
			defer func() { _, _ = sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, 0) }()
			_, err := conn.ExecContext(ctx, "vacuum")
			return err
		})
		return "", err
	case "optimize":
		err := s.withConn(ctx, app, func(conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "pragma optimize")
			return err
		})
		return "", err
	case "checkpoint":
		var busy, logFrames, checkpointed int
		err := s.withConn(ctx, app, func(conn *sql.Conn) error {
			return conn.QueryRowContext(ctx, "pragma wal_checkpoint(truncate)").Scan(&busy, &logFrames, &checkpointed)
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed), nil
	case "integrity_check":
		results := []string{}
		err := s.withConn(ctx, app, func(conn *sql.Conn) error {
			rows, err := conn.QueryContext(ctx, "pragma integrity_check")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				results = append(results, line)
			}
			return rows.Err()
		})
		if err != nil {
			return "", err
		}
		return strings.Join(results, "\n"), nil
	default:
		return "", fmt.Errorf("unknown maintenance op %q; use one of %s", op, storageMaintenanceOps)
	}
}
