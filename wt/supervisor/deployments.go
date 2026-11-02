package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	deploymentKeep       = 10
	deploymentTimeLayout = "20060102T150405.000000000Z"
	deploymentMetaName   = "meta.json"
	deploymentBlobDir    = ".blobs"
	deploymentReplayName = "idempotency.json"
	// deploymentReplayKeep bounds one app's persisted idempotency responses.
	deploymentReplayKeep = 64
	// deploymentReplayTTL is how long a deploy response may be replayed.
	deploymentReplayTTL      = 24 * time.Hour
	deploymentReplayVersion  = 1
	deploymentStatusOK       = "success"
	deploymentStatusFail     = "failed"
	deploymentStatusQueued   = "queued"
	deploymentStatusRunning  = "running"
	deploymentStatusCanceled = "canceled"
	// deploymentErrorBytes caps the failure text kept in history.
	deploymentErrorBytes = 4 << 10
	deploymentLogName    = "deploy.log"
	deploymentLogBytes   = 1 << 20
)

// deploymentMaxBytes bounds one app's retained history. Identical artifacts are shared, so this is generous.
var deploymentMaxBytes int64 = 1 << 30

var errDeploymentNotFound = errors.New("deployment not found")

// deploymentInfo describes one retained bundle. Failed deploy attempts are retained as metadata only, without a bundle to roll back to.
type deploymentInfo struct {
	ID          string            `json:"id"`
	DeployedAt  time.Time         `json:"deployedAt"`
	QueuedAt    time.Time         `json:"queuedAt"`
	StartedAt   time.Time         `json:"startedAt"`
	FinishedAt  time.Time         `json:"finishedAt"`
	Mode        string            `json:"mode,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	WasmDigest  string            `json:"wasmDigest,omitempty"`
	WasmBytes   int64             `json:"wasmBytes,omitempty"`
	Runtime     string            `json:"runtime,omitempty"`
	Target      string            `json:"target,omitempty"`
	Rebuildable bool              `json:"rebuildable"`
	Bytes       int64             `json:"bytes"`
	Blobs       map[string]string `json:"blobs,omitempty"`
	Static      map[string]string `json:"static,omitempty"`
	Status      string            `json:"status,omitempty"`
	Error       string            `json:"error,omitempty"`
	Current     bool              `json:"current,omitempty"`
}

// deploymentReplayFile is the on-disk form of one app's idempotency responses.
type deploymentReplayFile struct {
	Version int                          `json:"version"`
	Entries map[string]deployReplayEntry `json:"entries"`
}

// deploymentManager keeps a bounded history of app bundles under the state directory.
type deploymentManager struct {
	dir string
	mu  sync.Mutex
}

func newDeploymentManager(dir string) *deploymentManager {
	return &deploymentManager{dir: dir}
}

func (m *deploymentManager) appDir(app string) string {
	return filepath.Join(m.dir, app)
}

func (m *deploymentManager) blobDir(app string) string {
	return filepath.Join(m.appDir(app), deploymentBlobDir)
}

// validDeploymentID reports whether id is a single deployment timestamp.
func validDeploymentID(id string) error {
	if id == "" || filepath.Base(id) != id {
		return errDeploymentNotFound
	}
	if _, err := time.Parse(deploymentTimeLayout, id); err != nil {
		return errDeploymentNotFound
	}
	return nil
}

// newID reserves an unused timestamp id for a new history entry.
func (m *deploymentManager) newID(app string) (string, time.Time, error) {
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return "", time.Time{}, fmt.Errorf("deployment dir: %w", err)
	}
	now := time.Now().UTC()
	id := now.Format(deploymentTimeLayout)
	for exists(m.appDir(app), id) {
		now = now.Add(time.Nanosecond)
		id = now.Format(deploymentTimeLayout)
	}
	return id, now, nil
}

// replayPath is the idempotency file beside the app's deployment history.
func (m *deploymentManager) replayPath(app string) string {
	return filepath.Join(m.appDir(app), deploymentReplayName)
}

// readReplayFile loads the replay cache, returning the OS error when it does not exist.
func (m *deploymentManager) readReplayFile(app string) (deploymentReplayFile, error) {
	var file deploymentReplayFile
	data, err := os.ReadFile(m.replayPath(app))
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return deploymentReplayFile{}, err
	}
	if file.Entries == nil {
		file.Entries = map[string]deployReplayEntry{}
	}
	return file, nil
}

// replay returns a recent stored response for an idempotency key, or false when it is absent or expired.
func (m *deploymentManager) replay(app, key string, now time.Time) (deployReplayEntry, bool) {
	if key == "" {
		return deployReplayEntry{}, false
	}
	file, err := m.readReplayFile(app)
	if err != nil {
		return deployReplayEntry{}, false
	}
	entry, ok := file.Entries[key]
	if !ok || now.Sub(entry.At) > deploymentReplayTTL {
		return deployReplayEntry{}, false
	}
	return entry, true
}

// storeReplay records an idempotency key, pruning expired and over-limit entries.
func (m *deploymentManager) storeReplay(app, key string, entry deployReplayEntry) error {
	if key == "" {
		return nil
	}
	file, err := m.readReplayFile(app)
	if err != nil {
		file = deploymentReplayFile{Version: deploymentReplayVersion, Entries: map[string]deployReplayEntry{}}
	}
	if file.Entries == nil {
		file.Entries = map[string]deployReplayEntry{}
	}
	file.Version = deploymentReplayVersion

	now := time.Now()
	for name, existing := range file.Entries {
		if now.Sub(existing.At) > deploymentReplayTTL {
			delete(file.Entries, name)
		}
	}
	file.Entries[key] = entry
	if len(file.Entries) > deploymentReplayKeep {
		type agedEntry struct {
			key string
			at  time.Time
		}
		aged := make([]agedEntry, 0, len(file.Entries))
		for name, existing := range file.Entries {
			aged = append(aged, agedEntry{key: name, at: existing.At})
		}
		sort.Slice(aged, func(i, j int) bool { return aged[i].at.Before(aged[j].at) })
		for _, item := range aged[:len(aged)-deploymentReplayKeep] {
			delete(file.Entries, item.key)
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return err
	}
	return writeFileAtomicMode(m.replayPath(app), append(data, '\n'), 0o600)
}

// clearReplay drops the app's replay cache.
func (m *deploymentManager) clearReplay(app string) error {
	err := os.Remove(m.replayPath(app))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ids lists deployment ids in oldest-first timestamp order.
func (m *deploymentManager) ids(app string) ([]string, error) {
	entries, err := os.ReadDir(m.appDir(app))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validDeploymentID(entry.Name()) != nil {
			continue
		}
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// list returns deployment records newest first, skipping unreadable entries.
func (m *deploymentManager) list(app string) ([]deploymentInfo, error) {
	ids, err := m.ids(app)
	if err != nil {
		return nil, err
	}
	out := make([]deploymentInfo, 0, len(ids))
	for _, id := range ids {
		data, err := os.ReadFile(filepath.Join(m.appDir(app), id, deploymentMetaName))
		if err != nil {
			continue
		}
		var info deploymentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		out = append(out, info)
	}
	// ids are oldest first; return newest first.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *deploymentManager) info(app, id string) (deploymentInfo, error) {
	if err := validDeploymentID(id); err != nil {
		return deploymentInfo{}, err
	}
	data, err := os.ReadFile(filepath.Join(m.appDir(app), id, deploymentMetaName))
	if err != nil {
		return deploymentInfo{}, errDeploymentNotFound
	}
	var info deploymentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return deploymentInfo{}, errDeploymentNotFound
	}
	return info, nil
}

// createRun reserves a deployment id before the install starts, so a caller can follow status and logs by URL.
func (m *deploymentManager) createRun(app, digest string) (deploymentInfo, error) {
	id, now, err := m.newID(app)
	if err != nil {
		return deploymentInfo{}, err
	}
	dir := filepath.Join(m.appDir(app), id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return deploymentInfo{}, err
	}
	info := deploymentInfo{
		ID:         id,
		DeployedAt: now,
		QueuedAt:   now,
		Digest:     digest,
		Status:     deploymentStatusQueued,
	}
	data, err := json.Marshal(info)
	if err != nil {
		_ = os.RemoveAll(dir)
		return deploymentInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, deploymentMetaName), append(data, '\n'), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return deploymentInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, deploymentLogName), nil, 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return deploymentInfo{}, err
	}
	return info, nil
}

// updateRun mutates one run's metadata atomically. The runner and a cancel request may both update it.
func (m *deploymentManager) updateRun(app, id string, update func(*deploymentInfo)) (deploymentInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, err := m.info(app, id)
	if err != nil {
		return deploymentInfo{}, err
	}
	update(&info)
	data, err := json.Marshal(info)
	if err != nil {
		return deploymentInfo{}, err
	}
	path := filepath.Join(m.appDir(app), id, deploymentMetaName)
	if err := writeFileAtomicMode(path, append(data, '\n'), 0o644); err != nil {
		return deploymentInfo{}, err
	}
	return info, nil
}

func (m *deploymentManager) logPath(app, id string) string {
	return filepath.Join(m.appDir(app), id, deploymentLogName)
}

// appendLog keeps a deployment's log bounded and never lets logging fail the install.
func (m *deploymentManager) appendLog(app, id string, data []byte) {
	path := m.logPath(app, id)
	info, err := os.Stat(path)
	if err == nil && info.Size()+int64(len(data)) > deploymentLogBytes {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = file.Write(data)
	_ = file.Close()
}

func (m *deploymentManager) readLog(app, id string) ([]byte, error) {
	if err := validDeploymentID(id); err != nil {
		return nil, errDeploymentNotFound
	}
	data, err := os.ReadFile(m.logPath(app, id))
	if os.IsNotExist(err) {
		return nil, errDeploymentNotFound
	}
	return data, err
}

// bundle returns the retained directory for a successful deployment that has a component.
func (m *deploymentManager) bundle(app, id string) (string, error) {
	if err := validDeploymentID(id); err != nil {
		return "", err
	}
	dir := filepath.Join(m.appDir(app), id)
	if data, err := os.ReadFile(filepath.Join(dir, deploymentMetaName)); err == nil {
		var info deploymentInfo
		if json.Unmarshal(data, &info) == nil && info.Status != "" && info.Status != deploymentStatusOK {
			return "", errDeploymentNotFound
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "app.json")); err != nil || !info.Mode().IsRegular() {
		return "", errDeploymentNotFound
	}
	for _, name := range []string{"main.cwasm", "main.wasm"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return dir, nil
		}
	}
	return "", errDeploymentNotFound
}

// content returns the app's content-addressed store.
func (m *deploymentManager) content(app string) *contentStore {
	return newContentStore(m.blobDir(app), m.blobDir(app))
}

// storeBlob puts a file in the content-addressed store and returns its digest and size.
func (m *deploymentManager) storeBlob(app, source string) (string, int64, error) {
	digest, size, _, err := m.content(app).putFile(source)
	return digest, size, err
}

// gcBlobs removes stored artifacts no retained bundle references.
func (m *deploymentManager) gcBlobs(app string) error {
	ids, err := m.ids(app)
	if err != nil {
		return err
	}
	referenced := map[string]struct{}{}
	for _, id := range ids {
		data, err := os.ReadFile(filepath.Join(m.appDir(app), id, deploymentMetaName))
		if err != nil {
			continue
		}
		var info deploymentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		for _, digest := range info.Blobs {
			referenced[digest] = struct{}{}
		}
		for _, digest := range info.Static {
			referenced[digest] = struct{}{}
		}
	}

	return m.content(app).prune(referenced)
}

type fileKey struct {
	device, inode uint64
}

// usage counts the bytes stored for one app, following hardlinks only once.
func (m *deploymentManager) usage(app string) (int64, error) {
	var total int64
	seen := map[fileKey]struct{}{}
	err := filepath.WalkDir(m.appDir(app), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			key := fileKey{device: uint64(stat.Dev), inode: stat.Ino}
			if _, ok := seen[key]; ok {
				return nil
			}
			seen[key] = struct{}{}
		}
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// prune keeps the newest bundles within both the count and byte limits, then drops unreferenced blobs.
func (m *deploymentManager) prune(app string) error {
	ids, err := m.ids(app)
	if err != nil {
		return err
	}
	// Never prune a deployment that is still queued or running; its status and log are live.
	removeOldest := func() (bool, error) {
		for i, id := range ids {
			info, err := m.info(app, id)
			if err != nil {
				continue
			}
			if info.Status == deploymentStatusQueued || info.Status == deploymentStatusRunning {
				continue
			}
			if err := os.RemoveAll(filepath.Join(m.appDir(app), id)); err != nil {
				return false, err
			}
			ids = append(ids[:i], ids[i+1:]...)
			return true, nil
		}
		return false, nil
	}
	for len(ids) > deploymentKeep {
		removed, err := removeOldest()
		if err != nil {
			return err
		}
		if !removed {
			break
		}
	}
	for len(ids) > 1 {
		used, err := m.usage(app)
		if err != nil {
			return err
		}
		if used <= deploymentMaxBytes {
			break
		}
		removed, err := removeOldest()
		if err != nil {
			return err
		}
		if !removed {
			break
		}
		if err := m.gcBlobs(app); err != nil {
			return err
		}
	}
	return m.gcBlobs(app)
}
