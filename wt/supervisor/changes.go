package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	rows    []rowChange
	omitted int
	bytes   int
}

func preUpdateValues(data sqlite.SQLitePreUpdateData, new bool) ([]any, error) {
	count := data.Count()
	values := make([]any, count)
	var err error
	if new {
		err = data.New(values...)
	} else {
		err = data.Old(values...)
	}
	if err != nil {
		return nil, err
	}
	encoded := make([]any, count)
	for i, value := range values {
		encoded[i] = encodeStorageValue(value)
	}
	return encoded, nil
}

func collectRowChange(data sqlite.SQLitePreUpdateData) (rowChange, error) {
	change := rowChange{Table: data.TableName, Columns: []string{}}
	switch data.Op {
	case sqlite3.SQLITE_INSERT:
		change.Op = "insert"
		change.RowID = data.NewRowID
		newValues, err := preUpdateValues(data, true)
		if err != nil {
			return rowChange{}, err
		}
		change.New = newValues
	case sqlite3.SQLITE_DELETE:
		change.Op = "delete"
		change.RowID = data.OldRowID
		oldValues, err := preUpdateValues(data, false)
		if err != nil {
			return rowChange{}, err
		}
		change.Old = oldValues
	case sqlite3.SQLITE_UPDATE:
		change.Op = "update"
		change.RowID = data.OldRowID
		if change.RowID == 0 {
			change.RowID = data.NewRowID
		}
		oldValues, err := preUpdateValues(data, false)
		if err != nil {
			return rowChange{}, err
		}
		newValues, err := preUpdateValues(data, true)
		if err != nil {
			return rowChange{}, err
		}
		change.Old = oldValues
		change.New = newValues
	default:
		return rowChange{}, fmt.Errorf("unknown pre-update operation %d", data.Op)
	}
	return change, nil
}

// changeValueSize measures encoded values; blobs arrive as a base64 wrapper, which storageValueSize does not know about.
func changeValueSize(value any) int {
	if blob, ok := value.(map[string]string); ok {
		return len(blob["$b64"])
	}
	return storageValueSize(value)
}

func rowChangeSize(change rowChange) int {
	size := len(change.Table) + len(change.Op) + 16
	for _, value := range change.Old {
		size += changeValueSize(value)
	}
	for _, value := range change.New {
		size += changeValueSize(value)
	}
	return size
}

func (c *rowChangeCollector) add(data sqlite.SQLitePreUpdateData) {
	if len(c.rows) >= maxCollectedChanges || c.bytes >= maxCollectedRowBytes {
		c.omitted++
		return
	}
	change, err := collectRowChange(data)
	if err != nil {
		log.Printf("storage: pre-update hook: %v", err)
		c.omitted++
		return
	}
	size := rowChangeSize(change)
	if c.bytes+size > maxCollectedRowBytes {
		c.omitted++
		c.bytes = maxCollectedRowBytes
		return
	}
	c.rows = append(c.rows, change)
	c.bytes += size
}

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
		registered := false
		if err := registerRowHook(conn, collector); err != nil {
			log.Printf("storage %s: pre-update hook: %v", app, err)
		} else {
			registered = true
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
