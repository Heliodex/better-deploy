package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	backupConfigName = "config.json"
	backupTreeName   = "tree.json"
	backupFilePrefix = "storage-"
	backupFileSuffix = ".db"
	backupDataSuffix = ".data.tar.gz"
	backupTempSuffix = ".tmp"

	backupKindManual    = "manual"
	backupKindAutomatic = "auto"
	backupKindImport    = "import"
	// backupKindLegacy marks snapshots that predate the tree index, so the panel can show them as plain backups.
	backupKindLegacy = ""

	defaultBackupEvery = "24h"
	defaultBackupKeep  = 7
	minBackupEvery     = time.Minute
	maxBackupEvery     = 30 * 24 * time.Hour
	minBackupKeep      = 1
	maxBackupKeep      = 100
	// backupCheckInterval is how often the manager looks for apps whose next snapshot is due. Intervals are stored as durations, so this only bounds how late a snapshot can be.
	backupCheckInterval = time.Minute
	// backupCheckJitter spreads the due check so several supervisors do not all archive at the same second.
	backupCheckJitter = backupCheckInterval / 5
	// backupWorkers bounds how many apps archive at once. Each app still has its own lock, so a large archive only delays that app's own backups.
	backupWorkers = 2
	// backupCheckTimeout bounds the integrity check on a snapshot before it is restored. It is longer than the query timeout because quick_check reads the whole database.
	backupCheckTimeout = 30 * time.Second
)

// backupTimeLayout names each snapshot by the UTC instant it was taken, so the name alone survives a missing or odd modification time.
const backupTimeLayout = "20060102T150405.000000000Z"

var (
	errNoStorageDatabase = errors.New("app has no storage database yet")
	errBackupNotFound    = errors.New("backup not found")
	errInvalidBackupName = errors.New("invalid backup name")
	errBackupUnchanged   = errors.New("database has not changed since the current snapshot")
)

// backupSettings is one app's snapshot policy. Snapshots are on by default so a fresh deployment is protected without configuration.
type backupSettings struct {
	Enabled bool   `json:"enabled"`
	Every   string `json:"every"`
	Keep    int    `json:"keep"`
}

func defaultBackupSettings() backupSettings {
	return backupSettings{Enabled: true, Every: defaultBackupEvery, Keep: defaultBackupKeep}
}

func (b backupSettings) interval() (time.Duration, error) {
	every, err := time.ParseDuration(b.Every)
	if err != nil {
		return 0, fmt.Errorf("every %q: use a duration such as 6h or 24h", b.Every)
	}

	if every < minBackupEvery || every > maxBackupEvery {
		return 0, fmt.Errorf("every %q must be between %s and %s", b.Every, minBackupEvery, maxBackupEvery)
	}

	return every, nil
}

func (b backupSettings) validate() error {
	if _, err := b.interval(); err != nil {
		return err
	}

	if b.Keep < minBackupKeep || b.Keep > maxBackupKeep {
		return fmt.Errorf("keep must be between %d and %d", minBackupKeep, maxBackupKeep)
	}

	return nil
}

// backupInfo describes one snapshot on disk and its place in the app's backup tree.
type backupInfo struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	DataSize  int64     `json:"dataSize,omitempty"`
	HasData   bool      `json:"hasData,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Parent    string    `json:"parent"`
	Kind      string    `json:"kind"`
}

// backupNode is the tree metadata kept beside the snapshot files. A snapshot's parent is the snapshot the database was based on when it was taken, so reverting and taking another snapshot branches the tree.
type backupNode struct {
	Parent string `json:"parent,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// Hash is the SHA-256 of the snapshot file, used to reject a snapshot that would duplicate the current node.
	Hash string `json:"hash,omitempty"`
}

// backupTree records each snapshot's parent and the snapshot the live database currently follows.
type backupTree struct {
	Current string `json:"current,omitempty"`
	// Checked is the last time a snapshot was made or found unchanged. It keeps the scheduler from retrying an unchanged database every minute.
	Checked time.Time             `json:"checked"`
	Nodes   map[string]backupNode `json:"nodes"`
}

// backupState is the snapshot files and their tree metadata, always loaded together.
type backupState struct {
	Files []backupInfo
	Tree  backupTree
}

// backupsResponse is the state the admin panel needs for an app's backup section.
type backupsResponse struct {
	Settings backupSettings `json:"settings"`
	Backups  []backupInfo   `json:"backups"`
	Database bool           `json:"database"`
	Current  string         `json:"current"`
	// Created is set only by a POST that made a snapshot; it stays empty when the database was unchanged.
	Created string `json:"created,omitempty"`
}

// backupManager owns per-app snapshot policies and files. Snapshots live outside the storage directory so an app can never read or delete them through its SQL service.
type backupManager struct {
	dir, dataDir string
	storage      *storageServer

	// locksMu guards locks. Each app has its own mutex, so a large /data archive, export or restore for one app does not block another app's backups.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func newBackupManager(dir string, storage *storageServer, dataDir string) *backupManager {
	return &backupManager{dir: dir, dataDir: dataDir, storage: storage, locks: map[string]*sync.Mutex{}}
}

// appLock returns the mutex for one app, creating it on first use.
func (m *backupManager) appLock(app string) *sync.Mutex {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()

	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}

	lock := m.locks[app]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[app] = lock
	}

	return lock
}

func (m *backupManager) lock(app string) {
	m.appLock(app).Lock()
}

func (m *backupManager) unlock(app string) {
	m.appLock(app).Unlock()
}

func (m *backupManager) appDir(app string) string {
	return filepath.Join(m.dir, app)
}

func (m *backupManager) configPath(app string) string {
	return filepath.Join(m.appDir(app), backupConfigName)
}

func (m *backupManager) treePath(app string) string {
	return filepath.Join(m.appDir(app), backupTreeName)
}

// dataPath is the /data archive kept beside a database snapshot.
func (m *backupManager) dataPath(app, name string) string {
	base := strings.TrimSuffix(name, backupFileSuffix)
	return filepath.Join(m.appDir(app), base+backupDataSuffix)
}

// content returns the app's content-addressed backup store.
func (m *backupManager) content(app string) *contentStore {
	return newContentStore(filepath.Join(m.appDir(app), ".blobs"), m.appDir(app))
}

// adoptBackupFile moves a staged snapshot file into the content store and links it at its public name.
func (m *backupManager) adoptBackupFile(app, source, dst string) error {
	digest, _, _, err := m.content(app).adoptFile(source)
	if err != nil {
		return err
	}

	return m.content(app).link(digest, dst)
}

// gcBlobs removes stored snapshot files that no public snapshot name references.
func (m *backupManager) gcBlobs(app string) error {
	entries, err := os.ReadDir(m.appDir(app))
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	referenced := map[string]struct{}{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, backupFileSuffix) && !strings.HasSuffix(name, backupDataSuffix)) {
			continue
		}

		digest, err := fileDigest(filepath.Join(m.appDir(app), name))
		if err != nil {
			continue
		}

		referenced[digest] = struct{}{}
	}

	return m.content(app).prune(referenced)
}

// settings reads an app's policy, falling back to the defaults when it has never been configured.
func (m *backupManager) settings(app string) (backupSettings, error) {
	settings := defaultBackupSettings()

	data, err := os.ReadFile(m.configPath(app))
	switch {
	case os.IsNotExist(err):
		return settings, nil
	case err != nil:
		return settings, err
	}

	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("backup config: %w", err)
	}

	if err := settings.validate(); err != nil {
		return settings, fmt.Errorf("backup config: %w", err)
	}

	return settings, nil
}

// writeFileAtomic replaces path through a temp file, so a crash leaves the previous contents intact.
func writeFileAtomic(path string, data []byte) error {
	return writeFileAtomicMode(path, data, 0o644)
}

// writeFileAtomicMode is writeFileAtomic with an explicit final mode, used for app.json in untrusted mode.
func writeFileAtomicMode(path string, data []byte, perm os.FileMode) error {
	temp := path + backupTempSuffix
	if err := os.WriteFile(temp, data, perm); err != nil {
		return err
	}

	if err := os.Chmod(temp, perm); err != nil {
		_ = os.Remove(temp)
		return err
	}

	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}

	return nil
}

// backupTime parses the instant from a snapshot file name.
func backupTime(name string) (time.Time, bool) {
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, backupFilePrefix), backupFileSuffix)
	created, err := time.Parse(backupTimeLayout, stamp)

	return created, err == nil
}

// filesLocked reads the snapshot files without their tree metadata, newest first.
func (m *backupManager) filesLocked(app string) ([]backupInfo, error) {
	entries, err := os.ReadDir(m.appDir(app))
	if os.IsNotExist(err) {
		return []backupInfo{}, nil
	}

	if err != nil {
		return nil, err
	}

	infos := make([]backupInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		switch {
		case strings.HasPrefix(name, backupFilePrefix) && strings.HasSuffix(name, backupFileSuffix):
			created, ok := backupTime(name)
			if !ok {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				return nil, err
			}

			backup := backupInfo{Name: name, Size: info.Size(), CreatedAt: created}
			if data, err := os.Stat(m.dataPath(app, name)); err == nil && data.Mode().IsRegular() {
				backup.DataSize = data.Size()
				backup.HasData = true
			}

			infos = append(infos, backup)
		case strings.HasPrefix(name, backupFilePrefix) && strings.HasSuffix(name, backupDataSuffix):
			// A data-only snapshot has no database file; the archive itself is the snapshot.
			name = strings.TrimSuffix(name, backupDataSuffix) + backupFileSuffix
			if exists(m.appDir(app), name) {
				continue
			}

			created, ok := backupTime(name)
			if !ok {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				return nil, err
			}

			infos = append(infos, backupInfo{Name: name, DataSize: info.Size(), HasData: true, CreatedAt: created})
		}
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.After(infos[j].CreatedAt) })

	return infos, nil
}

// stateLocked loads snapshot files and their tree index together, rebuilding the index when it is missing, stale or broken.
func (m *backupManager) stateLocked(app string) (backupState, error) {
	files, err := m.filesLocked(app)
	if err != nil {
		return backupState{}, err
	}

	tree, changed, err := m.loadTreeLocked(app, files)
	if err != nil {
		return backupState{}, err
	}

	if changed {
		if err := m.saveTreeLocked(app, tree); err != nil {
			return backupState{}, err
		}
	}

	for i := range files {
		node := tree.Nodes[files[i].Name]
		files[i].Parent = node.Parent
		files[i].Kind = node.Kind
	}

	return backupState{Files: files, Tree: tree}, nil
}

// listLocked returns snapshots while the caller holds the app lock.
func (m *backupManager) listLocked(app string) ([]backupInfo, error) {
	state, err := m.stateLocked(app)
	if err != nil {
		return nil, err
	}

	return state.Files, nil
}

// list returns an app's snapshots, newest first.
func (m *backupManager) list(app string) ([]backupInfo, error) {
	m.lock(app)
	defer m.unlock(app)

	return m.listLocked(app)
}

// overview returns everything the panel shows for one app.
func (m *backupManager) overview(app string) (backupsResponse, error) {
	m.lock(app)
	defer m.unlock(app)

	settings, err := m.settings(app)
	if err != nil {
		return backupsResponse{}, err
	}

	state, err := m.stateLocked(app)
	if err != nil {
		return backupsResponse{}, err
	}

	return backupsResponse{
		Settings: settings,
		Backups:  state.Files,
		Database: m.storage.hasDatabase(app),
		Current:  state.Tree.Current,
	}, nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func (m *backupManager) saveSettingsLocked(app string, settings backupSettings) error {
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	if err := writeFileAtomic(m.configPath(app), append(data, '\n')); err != nil {
		return fmt.Errorf("write backup config: %w", err)
	}

	return m.pruneLocked(app, settings.Keep)
}

// saveSettings validates and persists an app's policy, then prunes snapshots the new keep count no longer covers.
func (m *backupManager) saveSettings(app string, settings backupSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}

	m.lock(app)
	defer m.unlock(app)

	return m.saveSettingsLocked(app, settings)
}

// create takes a snapshot now even when automatic backups are disabled.
func (m *backupManager) create(ctx context.Context, app string) (backupInfo, error) {
	m.lock(app)
	defer m.unlock(app)

	return m.createLocked(ctx, app, backupKindManual)
}

// backupIfDue takes a scheduled snapshot when the app's interval has elapsed and the database changed.
func (m *backupManager) backupIfDue(ctx context.Context, app string, now time.Time) error {
	m.lock(app)
	defer m.unlock(app)

	settings, err := m.settings(app)
	if err != nil {
		return err
	}

	if !settings.Enabled {
		return nil
	}

	every, err := settings.interval()
	if err != nil {
		return err
	}

	state, err := m.stateLocked(app)
	if err != nil {
		return err
	}

	last := state.Tree.Checked
	if len(state.Files) > 0 && state.Files[0].CreatedAt.After(last) {
		last = state.Files[0].CreatedAt
	}

	if !last.IsZero() && now.Sub(last) < every {
		return nil
	}

	_, err = m.createLocked(ctx, app, backupKindAutomatic)
	if errors.Is(err, errBackupUnchanged) {
		return nil
	}

	return err
}

// dueApps returns the app names the scheduler may snapshot. Apps live under the state directory, so this is the same set loadApps sees.
func (m *backupManager) dueApps() []string {
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("backup: scan apps: %v", err)
		}

		return nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !validName(entry.Name()) {
			continue
		}

		names = append(names, entry.Name())
	}

	return names
}

// runDue snapshots every app whose policy is enabled and whose newest snapshot is older than its interval. An app without a database still gets a data-only snapshot of /data. A small worker pool bounds concurrent archives without letting one app's large /data delay every other app.
func (m *backupManager) runDue(ctx context.Context, now time.Time) {
	names := m.dueApps()

	workers := min(backupWorkers, len(names))
	if workers == 0 {
		return
	}

	jobs := make(chan string)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for name := range jobs {
				if ctx.Err() != nil {
					return
				}

				if err := m.backupIfDue(ctx, name, now); err != nil {
					log.Printf("backup %s: %v", name, err)
				}
			}
		})
	}

	for _, name := range names {
		select {
		case jobs <- name:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()

			return
		}
	}

	close(jobs)
	wg.Wait()
}

// backupCheckDelay returns the next due check interval with up to a fifth of the interval of jitter in either direction.
func backupCheckDelay(draw func(int64) int64) time.Duration {
	span := int64(backupCheckJitter)*2 + 1
	return backupCheckInterval - backupCheckJitter + time.Duration(draw(span))
}

// loop runs the periodic snapshots until the context is cancelled.
func (m *backupManager) loop(ctx context.Context) {
	m.runDue(ctx, time.Now())

	for {
		timer := time.NewTimer(backupCheckDelay(rand.Int64N))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case now := <-timer.C:
			m.runDue(ctx, now)
		}
	}
}
