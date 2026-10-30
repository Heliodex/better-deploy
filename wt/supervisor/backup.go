package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
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
	backupTempSuffix = ".tmp"

	backupKindManual    = "manual"
	backupKindAutomatic = "auto"
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
	dir     string
	storage *storageServer

	// mu serialises snapshots, restores and settings changes. A restore must not race a snapshot of the same database, and the manager's work is small enough that one lock is simpler than per-app ones.
	mu sync.Mutex
}

func newBackupManager(dir string, storage *storageServer) *backupManager {
	return &backupManager{dir: dir, storage: storage}
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
	temp := path + backupTempSuffix
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// backupTreeAcyclic reports whether every parent chain ends at a root instead of looping.
func backupTreeAcyclic(tree backupTree) bool {
	for name := range tree.Nodes {
		seen := map[string]struct{}{}
		for id := name; id != ""; {
			if _, ok := seen[id]; ok {
				return false
			}
			seen[id] = struct{}{}
			node, ok := tree.Nodes[id]
			if !ok {
				break
			}
			id = node.Parent
		}
	}
	return true
}

func backupOldestRoot(tree backupTree) string {
	root := ""
	for name, node := range tree.Nodes {
		if node.Parent != "" {
			continue
		}
		if root == "" || name < root {
			root = name
		}
	}
	return root
}

// loadTreeLocked reads the index and reconciles it with the files on disk. Snapshots from before the index existed are chained in creation order.
func (m *backupManager) loadTreeLocked(app string, files []backupInfo) (backupTree, bool, error) {
	tree := backupTree{Nodes: map[string]backupNode{}}
	changed := false

	data, err := os.ReadFile(m.treePath(app))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &tree); err != nil {
			tree = backupTree{Nodes: map[string]backupNode{}}
			changed = true
		}
	case os.IsNotExist(err):
		changed = true
	default:
		return tree, false, err
	}
	if tree.Nodes == nil {
		tree.Nodes = map[string]backupNode{}
		changed = true
	}

	existing := make(map[string]struct{}, len(files))
	for _, file := range files {
		existing[file.Name] = struct{}{}
	}
	for name := range tree.Nodes {
		if _, ok := existing[name]; !ok {
			delete(tree.Nodes, name)
			changed = true
		}
	}

	chronological := append([]backupInfo(nil), files...)
	sort.Slice(chronological, func(i, j int) bool { return chronological[i].CreatedAt.Before(chronological[j].CreatedAt) })

	if len(tree.Nodes) == 0 {
		parent := ""
		for _, file := range chronological {
			tree.Nodes[file.Name] = backupNode{Parent: parent, Kind: backupKindLegacy}
			parent = file.Name
			changed = true
		}
	} else {
		// A file the index has not recorded yet, such as one written just before a crash, continues from the current node.
		parent := tree.Current
		if _, ok := tree.Nodes[parent]; !ok {
			parent = ""
		}
		for _, file := range chronological {
			if _, ok := tree.Nodes[file.Name]; ok {
				continue
			}
			tree.Nodes[file.Name] = backupNode{Parent: parent, Kind: backupKindLegacy}
			parent = file.Name
			changed = true
		}
	}

	for name, node := range tree.Nodes {
		if node.Parent == "" {
			continue
		}
		if _, ok := tree.Nodes[node.Parent]; !ok {
			node.Parent = ""
			tree.Nodes[name] = node
			changed = true
		}
	}
	if !backupTreeAcyclic(tree) {
		parent := ""
		for _, file := range chronological {
			tree.Nodes[file.Name] = backupNode{Parent: parent, Kind: tree.Nodes[file.Name].Kind}
			parent = file.Name
		}
		changed = true
	}

	// Keep one root so the graph starts in one place; any extra roots become children of the oldest.
	root := backupOldestRoot(tree)
	for name, node := range tree.Nodes {
		if node.Parent == "" && name != root {
			node.Parent = root
			tree.Nodes[name] = node
			changed = true
		}
	}

	if _, ok := tree.Nodes[tree.Current]; !ok {
		tree.Current = ""
		if len(chronological) > 0 {
			tree.Current = chronological[len(chronological)-1].Name
		}
		changed = true
	}
	return tree, changed, nil
}

func (m *backupManager) saveTreeLocked(app string, tree backupTree) error {
	if tree.Nodes == nil {
		tree.Nodes = map[string]backupNode{}
	}
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(m.treePath(app), append(data, '\n')); err != nil {
		return fmt.Errorf("write backup tree: %w", err)
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
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), backupFilePrefix) || !strings.HasSuffix(entry.Name(), backupFileSuffix) {
			continue
		}
		created, ok := backupTime(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, backupInfo{Name: entry.Name(), Size: info.Size(), CreatedAt: created})
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

func (m *backupManager) listLocked(app string) ([]backupInfo, error) {
	state, err := m.stateLocked(app)
	if err != nil {
		return nil, err
	}
	return state.Files, nil
}

// list returns an app's snapshots, newest first.
func (m *backupManager) list(app string) ([]backupInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(app)
}

// overview returns everything the panel shows for one app.
func (m *backupManager) overview(app string) (backupsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// backupHash fingerprints a snapshot file so an unchanged database is not recorded twice.
func backupHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// validBackupName ensures a name from the API is one of the manager's own snapshot files and cannot escape the app's backup directory.
func validBackupName(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || !strings.HasPrefix(name, backupFilePrefix) || !strings.HasSuffix(name, backupFileSuffix) {
		return "", errInvalidBackupName
	}
	if _, ok := backupTime(name); !ok {
		return "", errInvalidBackupName
	}
	return name, nil
}

// restore replaces the live database with a snapshot using SQLite's online backup API, so the file is never copied while a writer has it open.
func (m *backupManager) restore(ctx context.Context, app, name string) error {
	name, err := validBackupName(name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	path := filepath.Join(m.appDir(app), name)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return errBackupNotFound
	}
	if err := checkBackupDatabase(path); err != nil {
		return fmt.Errorf("backup is not a valid database: %w", err)
	}
	if err := m.storage.restoreFrom(ctx, app, path); err != nil {
		return err
	}
	state, err := m.stateLocked(app)
	if err != nil {
		return err
	}
	tree := state.Tree
	if _, ok := tree.Nodes[name]; !ok {
		tree.Nodes[name] = backupNode{Parent: tree.Current, Kind: backupKindLegacy}
	}
	// Later snapshots continue from the restored node, which is what makes the next backup a new branch.
	tree.Current = name
	if err := m.saveTreeLocked(app, tree); err != nil {
		return err
	}
	log.Printf("backup %s: restored %s", app, name)
	return nil
}

// backupPathNames is the current node and its ancestors, which pruning must not remove while another choice exists.
func backupPathNames(tree backupTree) map[string]struct{} {
	protected := map[string]struct{}{}
	for id := tree.Current; id != ""; {
		if _, seen := protected[id]; seen {
			break
		}
		protected[id] = struct{}{}
		node, ok := tree.Nodes[id]
		if !ok {
			break
		}
		id = node.Parent
	}
	return protected
}

// backupOldestLeaf returns the oldest node with no children outside the protected branch.
func backupOldestLeaf(tree backupTree, protected map[string]struct{}) string {
	children := map[string]int{}
	for _, node := range tree.Nodes {
		if node.Parent != "" {
			children[node.Parent]++
		}
	}
	oldest := ""
	for name := range tree.Nodes {
		if _, ok := protected[name]; ok {
			continue
		}
		if children[name] > 0 {
			continue
		}
		if oldest == "" || name < oldest {
			oldest = name
		}
	}
	return oldest
}

// backupPathChild returns the child of root that lies on the protected branch.
func backupPathChild(tree backupTree, root string, protected map[string]struct{}) string {
	for name, node := range tree.Nodes {
		if node.Parent != root {
			continue
		}
		if _, ok := protected[name]; ok {
			return name
		}
	}
	return ""
}

// pruneLocked removes the oldest snapshots beyond keep, keeping the branch the live database follows for as long as possible. Abandoned leaves go first; only when the current branch alone is too long is its oldest node dropped and its child promoted to root.
func (m *backupManager) pruneLocked(app string, keep int) error {
	state, err := m.stateLocked(app)
	if err != nil {
		return err
	}
	tree := state.Tree
	if len(tree.Nodes) <= keep {
		return nil
	}
	changed := false
	for len(tree.Nodes) > keep {
		protected := backupPathNames(tree)
		if leaf := backupOldestLeaf(tree, protected); leaf != "" {
			if err := os.Remove(filepath.Join(m.appDir(app), leaf)); err != nil && !os.IsNotExist(err) {
				return err
			}
			delete(tree.Nodes, leaf)
			changed = true
			continue
		}

		root := backupOldestRoot(tree)
		if root == "" || root == tree.Current {
			break
		}
		child := backupPathChild(tree, root, protected)
		if err := os.Remove(filepath.Join(m.appDir(app), root)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(tree.Nodes, root)
		if child != "" {
			node := tree.Nodes[child]
			node.Parent = ""
			tree.Nodes[child] = node
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return m.saveTreeLocked(app, tree)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveSettingsLocked(app, settings)
}

// createLocked snapshots the live database as a child of the current node. Reverting moves the current node, so the next snapshot branches from there.
func (m *backupManager) createLocked(ctx context.Context, app, kind string) (backupInfo, error) {
	if !m.storage.hasDatabase(app) {
		return backupInfo{}, errNoStorageDatabase
	}
	settings, err := m.settings(app)
	if err != nil {
		return backupInfo{}, err
	}
	state, err := m.stateLocked(app)
	if err != nil {
		return backupInfo{}, err
	}
	parent := state.Tree.Current
	if _, ok := state.Tree.Nodes[parent]; !ok {
		parent = ""
	}
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return backupInfo{}, fmt.Errorf("backup dir: %w", err)
	}

	now := time.Now().UTC()
	name := backupFilePrefix + now.Format(backupTimeLayout) + backupFileSuffix
	for exists(m.appDir(app), name) {
		now = now.Add(time.Nanosecond)
		name = backupFilePrefix + now.Format(backupTimeLayout) + backupFileSuffix
	}
	path := filepath.Join(m.appDir(app), name)
	temp := path + backupTempSuffix
	_ = os.Remove(temp)
	if err := m.storage.backupTo(ctx, app, temp); err != nil {
		_ = os.Remove(temp)
		return backupInfo{}, err
	}
	candidateHash, err := backupHash(temp)
	if err != nil {
		log.Printf("backup %s: hash candidate: %v", app, err)
	}

	tree := state.Tree
	parentHash := ""
	if parent != "" {
		node := tree.Nodes[parent]
		parentHash = node.Hash
		if parentHash == "" {
			// Snapshots from before hashes were recorded get one now, so the next attempt can compare cheaply.
			hash, err := backupHash(filepath.Join(m.appDir(app), parent))
			if err != nil {
				log.Printf("backup %s: hash current: %v", app, err)
			} else {
				parentHash = hash
				node.Hash = hash
				tree.Nodes[parent] = node
			}
		}
	}
	if candidateHash != "" && candidateHash == parentHash {
		_ = os.Remove(temp)
		tree.Checked = now
		if err := m.saveTreeLocked(app, tree); err != nil {
			log.Printf("backup %s: save tree: %v", app, err)
		}
		return backupInfo{}, errBackupUnchanged
	}

	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return backupInfo{}, fmt.Errorf("install backup: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return backupInfo{}, err
	}

	tree.Nodes[name] = backupNode{Parent: parent, Kind: kind, Hash: candidateHash}
	tree.Current = name
	tree.Checked = now
	if err := m.saveTreeLocked(app, tree); err != nil {
		// The file is already a valid snapshot; the next load will add it as a child of the previous current node.
		log.Printf("backup %s: save tree: %v", app, err)
	}
	if err := m.pruneLocked(app, settings.Keep); err != nil {
		log.Printf("backup %s: prune: %v", app, err)
	}
	return backupInfo{Name: name, Size: info.Size(), CreatedAt: now, Parent: parent, Kind: kind}, nil
}

// create takes a snapshot now even when automatic backups are disabled.
func (m *backupManager) create(ctx context.Context, app string) (backupInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(ctx, app, backupKindManual)
}

func (m *backupManager) backupIfDue(ctx context.Context, app string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// runDue snapshots every app whose policy is enabled and whose newest snapshot is older than its interval. Apps without a database are skipped; one appears after the app's first storage use.
func (m *backupManager) runDue(ctx context.Context, now time.Time) {
	entries, err := os.ReadDir(m.storage.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("backup: scan storage: %v", err)
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validName(entry.Name()) {
			continue
		}
		app := entry.Name()
		if !m.storage.hasDatabase(app) {
			continue
		}
		if err := m.backupIfDue(ctx, app, now); err != nil {
			log.Printf("backup %s: %v", app, err)
		}
	}
}

// loop runs the periodic snapshots until the context is cancelled.
func (m *backupManager) loop(ctx context.Context) {
	m.runDue(ctx, time.Now())
	ticker := time.NewTicker(backupCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.runDue(ctx, now)
		}
	}
}
