package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	changeLogName      = "changes.log"
	changeLogBackup    = "changes.log.1"
	maxChangeLogBytes  = 1 << 20
	maxChangeEntrySize = 256 << 10
	defaultChangeLimit = 200
	maxChangeLimit     = 1000
)

// storageChange is one successful write through the app-facing storage service.
type storageChange struct {
	Time         time.Time        `json:"time"`
	SQL          string           `json:"sql"`
	Params       []jsontext.Value `json:"params"`
	RowsAffected int64            `json:"rowsAffected"`
}

type storageChangesResponse struct {
	Changes []storageChange `json:"changes"`
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

// trimChange keeps a single statement from filling the log with a huge parameter blob.
func trimChange(change storageChange) storageChange {
	line, err := json.Marshal(change)
	if err != nil || len(line) <= maxChangeEntrySize {
		return change
	}
	change.Params = []jsontext.Value{jsontext.Value([]byte(`"<params omitted: too large>"`))}
	return change
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
	for i, j := 0, len(changes)-1; i < j; i, j = i+1, j-1 {
		changes[i], changes[j] = changes[j], changes[i]
	}
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
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	if s.lookup(name) == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	if s.storage == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
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
