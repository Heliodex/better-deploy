package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	changeLogName        = "changes.log"
	changeLogBackup      = "changes.log.1"
	maxChangeLogBytes    = 1 << 20
	maxChangeEntrySize   = 256 << 10
	maxCollectedChanges  = 500
	maxCollectedRowBytes = 256 << 10
	defaultChangeLimit   = 200
	maxChangeLimit       = 1000
)

// storageChange is one successful write through the app-facing storage service.
type storageChange struct {
	Time          time.Time        `json:"time"`
	RequestID     string           `json:"requestId,omitempty"`
	SQL           string           `json:"sql"`
	Params        []jsontext.Value `json:"params"`
	RowsAffected  int64            `json:"rowsAffected"`
	Rows          []rowChange      `json:"rows,omitempty"`
	RowsTruncated bool             `json:"rowsTruncated,omitempty"`
}

// rowChange is one row touched by a statement. Old is set for updates and deletes, New for inserts and updates.
type rowChange struct {
	Table   string   `json:"table"`
	Op      string   `json:"op"`
	RowID   int64    `json:"rowId,omitempty"`
	Columns []string `json:"columns"`
	Old     []any    `json:"old,omitempty"`
	New     []any    `json:"new,omitempty"`
}

// rowChangeCollector receives pre-update rows for one write operation.
type rowChangeCollector struct {
	rows           []rowChange
	omitted, bytes int
}

var errRowChangeTooLarge = errors.New("row change exceeds the capture budget")

// preUpdateValues fetches a row's values, checks their encoded size against the budget, and only then base64-encodes blobs.
func preUpdateValues(data sqlite.SQLitePreUpdateData, new bool, budget int) ([]any, int, error) {
	count := data.Count()
	values := make([]any, count)
	var err error
	if new {
		err = data.New(values...)
	} else {
		err = data.Old(values...)
	}
	if err != nil {
		return nil, 0, err
	}
	total := 0
	for _, value := range values {
		total += storageValueSize(value)
		if total > budget {
			return nil, total, errRowChangeTooLarge
		}
	}
	encoded := make([]any, count)
	for i, value := range values {
		encoded[i] = encodeStorageValue(value)
	}
	return encoded, total, nil
}

// collectRowChange builds one change entry for an insert, update or delete and returns its encoded size.
func collectRowChange(data sqlite.SQLitePreUpdateData, budget int) (rowChange, int, error) {
	change := rowChange{Table: data.TableName, Columns: []string{}}
	switch data.Op {
	case sqlite3.SQLITE_INSERT:
		change.Op = "insert"
		change.RowID = data.NewRowID
		newValues, size, err := preUpdateValues(data, true, budget)
		if err != nil {
			return rowChange{}, size, err
		}
		change.New = newValues
		return change, size, nil
	case sqlite3.SQLITE_DELETE:
		change.Op = "delete"
		change.RowID = data.OldRowID
		oldValues, size, err := preUpdateValues(data, false, budget)
		if err != nil {
			return rowChange{}, size, err
		}
		change.Old = oldValues
		return change, size, nil
	case sqlite3.SQLITE_UPDATE:
		change.Op = "update"
		change.RowID = data.OldRowID
		if change.RowID == 0 {
			change.RowID = data.NewRowID
		}
		oldValues, oldSize, err := preUpdateValues(data, false, budget)
		if err != nil {
			return rowChange{}, oldSize, err
		}
		newValues, newSize, err := preUpdateValues(data, true, budget-oldSize)
		if err != nil {
			return rowChange{}, oldSize + newSize, err
		}
		change.Old = oldValues
		change.New = newValues
		return change, oldSize + newSize, nil
	default:
		return rowChange{}, 0, fmt.Errorf("unknown pre-update operation %d", data.Op)
	}
}

// add appends a change while the count and byte budgets allow, counting anything larger as omitted.
func (c *rowChangeCollector) add(data sqlite.SQLitePreUpdateData) {
	if len(c.rows) >= maxCollectedChanges || c.bytes >= maxCollectedRowBytes {
		c.omitted++
		return
	}
	change, size, err := collectRowChange(data, maxCollectedRowBytes-c.bytes)
	if err != nil {
		if !errors.Is(err, errRowChangeTooLarge) {
			log.Printf("storage: pre-update hook: %v", err)
		}
		c.omitted++
		if errors.Is(err, errRowChangeTooLarge) {
			c.bytes = maxCollectedRowBytes
		}
		return
	}
	if c.bytes+size > maxCollectedRowBytes {
		c.omitted++
		c.bytes = maxCollectedRowBytes
		return
	}
	c.rows = append(c.rows, change)
	c.bytes += size
}

// tableColumns reads a table's column names through SQLite's table_xinfo pragma.
func tableColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "pragma table_xinfo("+quoteSQLiteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk, &hidden); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// resolveColumns adds column names to collected rows. It runs after the statement or transaction has finished, when the connection can run schema queries again.
func (c *rowChangeCollector) resolveColumns(ctx context.Context, conn *sql.Conn) {
	cache := map[string][]string{}
	for i := range c.rows {
		row := &c.rows[i]
		columns, ok := cache[row.Table]
		if !ok {
			resolved, err := tableColumns(ctx, conn, row.Table)
			if err != nil {
				log.Printf("storage: pre-update columns for %s: %v", row.Table, err)
			}
			columns = resolved
			cache[row.Table] = columns
		}
		row.Columns = columns
		needed := max(len(row.Old), len(row.New))
		for len(row.Columns) < needed {
			row.Columns = append(row.Columns, fmt.Sprintf("column_%d", len(row.Columns)+1))
		}
	}
}

// registerRowHook installs the driver's pre-update hook for one write. The hook is replaced on the next write, so it never accumulates callbacks.
func registerRowHook(conn *sql.Conn, collector *rowChangeCollector) error {
	return conn.Raw(func(driverConn any) error {
		hooker, ok := driverConn.(interface {
			RegisterPreUpdateHook(sqlite.PreUpdateHookFn)
		})
		if !ok {
			return nil
		}
		hooker.RegisterPreUpdateHook(func(data sqlite.SQLitePreUpdateData) {
			collector.add(data)
		})
		return nil
	})
}

// unregisterRowHook removes the pre-update hook from the connection.
func unregisterRowHook(conn *sql.Conn) error {
	return conn.Raw(func(driverConn any) error {
		if hooker, ok := driverConn.(interface {
			RegisterPreUpdateHook(sqlite.PreUpdateHookFn)
		}); ok {
			hooker.RegisterPreUpdateHook(nil)
		}
		return nil
	})
}

// withWriteConn runs a write with a pre-update collector attached to the connection. Columns are resolved after the statement or transaction finishes, before the connection is released.
func (s *storageServer) withWriteConn(ctx context.Context, app string, fn func(*sql.Conn, *rowChangeCollector) error) error {
	return s.withConn(ctx, app, func(conn *sql.Conn) error {
		collector := &rowChangeCollector{}
		registered := true
		if err := registerRowHook(conn, collector); err != nil {
			log.Printf("storage %s: pre-update hook: %v", app, err)
			registered = false
		}
		if registered {
			defer func() {
				if err := unregisterRowHook(conn); err != nil {
					log.Printf("storage %s: clear pre-update hook: %v", app, err)
				}
			}()
		}
		if err := fn(conn, collector); err != nil {
			return err
		}
		collector.resolveColumns(ctx, conn)
		return nil
	})
}

type storageChangesResponse struct {
	Changes []storageChange `json:"changes"`
}

// trimChange keeps a single statement from filling the log with a huge parameter blob.
func trimChange(change storageChange) storageChange {
	line, err := json.Marshal(change)
	if err != nil || len(line) <= maxChangeEntrySize {
		return change
	}
	if len(change.Params) > 0 {
		change.Params = []jsontext.Value{jsontext.Value([]byte(`"<params omitted: too large>"`))}
	}
	line, err = json.Marshal(change)
	if err != nil || len(line) <= maxChangeEntrySize {
		return change
	}
	if len(change.Rows) > 0 {
		change.Rows = nil
		change.RowsTruncated = true
	}
	return change
}

// logChanges appends successful statements to the app's change log. Logging is best effort: a full disk or bad permissions must not make the app's write fail.
func (s *storageServer) logChanges(app string, changes []storageChange) {
	if len(changes) == 0 {
		return
	}
	if s.bus != nil {
		for _, change := range changes {
			s.publishStorageChange(app, change)
		}
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()

	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("storage %s: change log: %v", app, err)
		return
	}
	path := filepath.Join(dir, changeLogName)

	var data []byte
	for _, change := range changes {
		if change.Params == nil {
			change.Params = []jsontext.Value{}
		}
		change = trimChange(change)
		line, err := json.Marshal(change)
		if err != nil {
			continue
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if len(data) == 0 {
		return
	}

	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(data)) > maxChangeLogBytes {
		_ = os.Remove(filepath.Join(dir, changeLogBackup))
		_ = os.Rename(path, filepath.Join(dir, changeLogBackup))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("storage %s: change log: %v", app, err)
		return
	}
	if _, err := file.Write(data); err != nil {
		log.Printf("storage %s: change log: %v", app, err)
	}
	if err := file.Close(); err != nil {
		log.Printf("storage %s: change log: %v", app, err)
	}
}

type storageBusEvent struct {
	RequestID    string   `json:"requestId,omitempty"`
	RowsAffected int64    `json:"rowsAffected"`
	Tables       []string `json:"tables,omitempty"`
	Ops          []string `json:"ops,omitempty"`
}

// publishStorageChange sends a compact trigger on the reserved storage topic after a committed write.
// It deliberately leaves the SQL and row values out of the bus; the browser should refetch through the app.
func (s *storageServer) publishStorageChange(app string, change storageChange) {
	event := storageBusEvent{RequestID: change.RequestID, RowsAffected: change.RowsAffected}
	seen := map[string]struct{}{}
	for _, row := range change.Rows {
		key := row.Table + "\x00" + row.Op
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		event.Tables = append(event.Tables, row.Table)
		event.Ops = append(event.Ops, row.Op)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, _ = s.bus.publish(app, "storage", jsontext.Value(data))
}

// recentChanges returns the newest entries first. The log keeps one rotated file, so the read is bounded by the two files.
func (s *storageServer) recentChanges(app string, limit int) ([]storageChange, error) {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()

	dir := filepath.Join(s.dir, app)
	changes := []storageChange{}
	for _, name := range []string{changeLogBackup, changeLogName} {
		path := filepath.Join(dir, name)
		file, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), maxChangeLogBytes)
		for scanner.Scan() {
			var change storageChange
			if err := json.Unmarshal(scanner.Bytes(), &change); err != nil {
				continue
			}
			if change.Params == nil {
				change.Params = []jsontext.Value{}
			}
			changes = append(changes, change)
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return nil, err
		}
	}

	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Time.Before(changes[j].Time) })
	if len(changes) > limit {
		changes = changes[len(changes)-limit:]
	}
	slices.Reverse(changes)
	return changes, nil
}

// changeLimitParam parses the change-log limit and bounds it to the accepted range.
func changeLimitParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultChangeLimit, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maxChangeLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxChangeLimit))
		return 0, false
	}
	return parsed, true
}

func (s *supervisor) handleDatabaseChanges(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.requireStorage(w, r, name) {
		return
	}
	limit, ok := changeLimitParam(w, r)
	if !ok {
		return
	}
	changes, err := s.storage.recentChanges(name, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, storageChangesResponse{Changes: changes})
}
