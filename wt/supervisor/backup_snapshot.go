// Snapshot creation, staging and restore for the backup manager.

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// snapshotHash fingerprints a snapshot's database and data archive so an unchanged snapshot is not recorded twice. Missing files are part of the hash so legacy database-only snapshots differ from new ones.
func snapshotHash(paths ...string) (string, error) {
	hash := sha256.New()
	for _, path := range paths {
		file, err := os.Open(path)
		if os.IsNotExist(err) {
			_, _ = hash.Write([]byte("missing\x00"))
			continue
		}
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// archiveData writes a tar.gz of the app's /data directory. A missing directory becomes an empty archive.
func archiveData(src, dst string) error {
	file, err := os.Create(dst)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	writeErr := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == src {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !entry.IsDir() && !info.Mode().IsRegular() {
			log.Printf("backup: skipping non-regular %s", rel)
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if entry.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(tw, in, info.Size())
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if writeErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		_ = os.Remove(dst)
		return writeErr
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = file.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := gz.Close(); err != nil {
		_ = file.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
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

func (m *backupManager) removeSnapshot(app, name string) error {
	if err := os.Remove(filepath.Join(m.appDir(app), name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(m.dataPath(app, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// stageSnapshot extracts /data and, when withDB is set, copies the snapshot database to a temporary file. Both are staged under dataDir so the caller can swap them into place.
func (m *backupManager) stageSnapshot(app, name string, withDB bool) (dataDir string, hasData bool, dbFile string, err error) {
	name, err = validBackupName(name)
	if err != nil {
		return "", false, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	database := filepath.Join(m.appDir(app), name)
	hasDatabase := false
	if info, err := os.Stat(database); err == nil && info.Mode().IsRegular() {
		if err := checkBackupDatabase(database); err != nil {
			return "", false, "", fmt.Errorf("backup is not a valid database: %w", err)
		}
		hasDatabase = true
	} else if err != nil && !os.IsNotExist(err) {
		return "", false, "", err
	}
	archive := m.dataPath(app, name)
	hasData = false
	if info, err := os.Stat(archive); err == nil && info.Mode().IsRegular() {
		hasData = true
	} else if err != nil && !os.IsNotExist(err) {
		return "", false, "", err
	}
	if !hasDatabase && !hasData {
		return "", false, "", errBackupNotFound
	}
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return "", hasData, "", err
	}
	if hasData {
		dataDir, err = os.MkdirTemp(m.dataDir, "."+app+".restore-")
		if err != nil {
			return "", true, "", err
		}
		if err := extractTarGzLimited(archive, dataDir, maxBackupExtractBytes, maxBackupTarFiles); err != nil {
			_ = os.RemoveAll(dataDir)
			return "", true, "", err
		}
	}
	if withDB && hasDatabase {
		temp, err := os.CreateTemp(m.dataDir, "."+app+".db-*")
		if err != nil {
			_ = os.RemoveAll(dataDir)
			return "", hasData, "", err
		}
		dbFile = temp.Name()
		if err := temp.Close(); err != nil {
			_ = os.Remove(dbFile)
			_ = os.RemoveAll(dataDir)
			return "", hasData, "", err
		}
		if err := copyFile(database, dbFile); err != nil {
			_ = os.Remove(dbFile)
			_ = os.RemoveAll(dataDir)
			return "", hasData, "", err
		}
	}
	return dataDir, hasData, dbFile, nil
}

// stageData extracts a snapshot's /data archive. Database snapshots keep their online-backup path in restore.
func (m *backupManager) stageData(app, name string) (string, bool, error) {
	dataDir, hasData, dbFile, err := m.stageSnapshot(app, name, false)
	if dbFile != "" {
		_ = os.Remove(dbFile)
	}
	return dataDir, hasData, err
}

// markCurrent makes a snapshot the branch future automatic snapshots continue from.
func (m *backupManager) markCurrent(app, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markCurrentLocked(app, name)
}

// markCurrentLocked is markCurrent for callers already holding the backup lock.
func (m *backupManager) markCurrentLocked(app, name string) error {
	state, err := m.stateLocked(app)
	if err != nil {
		return err
	}
	tree := state.Tree
	if _, ok := tree.Nodes[name]; !ok {
		tree.Nodes[name] = backupNode{Parent: tree.Current, Kind: backupKindLegacy}
	}
	tree.Current = name
	return m.saveTreeLocked(app, tree)
}

// installData replaces an app's /data directory with a staged snapshot.
func (m *backupManager) installData(app, temp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return replaceDir(temp, filepath.Join(m.dataDir, app))
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
	hasDatabase := false
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		if err := checkBackupDatabase(path); err != nil {
			return fmt.Errorf("backup is not a valid database: %w", err)
		}
		hasDatabase = true
	} else if err != nil && !os.IsNotExist(err) {
		return err
	} else if _, err := os.Stat(m.dataPath(app, name)); err != nil {
		return errBackupNotFound
	}
	if hasDatabase {
		if err := m.storage.restoreFrom(ctx, app, path); err != nil {
			return err
		}
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

// backupName picks an unused snapshot name, stepping it forward when two land in the same nanosecond.
func (m *backupManager) backupName(app string, now time.Time) (string, time.Time) {
	name := backupFilePrefix + now.Format(backupTimeLayout) + backupFileSuffix
	for exists(m.appDir(app), name) {
		now = now.Add(time.Nanosecond)
		name = backupFilePrefix + now.Format(backupTimeLayout) + backupFileSuffix
	}
	return name, now
}

// parentSnapshotHash returns the current node's hash, recording one for snapshots that predate hashes.
func (m *backupManager) parentSnapshotHash(app, parent string, tree *backupTree) string {
	node := tree.Nodes[parent]
	if node.Hash != "" {
		return node.Hash
	}
	hash, err := snapshotHash(filepath.Join(m.appDir(app), parent), m.dataPath(app, parent))
	if err != nil {
		log.Printf("backup %s: hash current: %v", app, err)
		return ""
	}
	node.Hash = hash
	tree.Nodes[parent] = node
	return hash
}

// createLocked snapshots the live database as a child of the current node. Reverting moves the current node, so the next snapshot branches from there.
func (m *backupManager) createLocked(ctx context.Context, app, kind string) (backupInfo, error) {
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
	name, now := m.backupName(app, now)
	path := filepath.Join(m.appDir(app), name)
	dataPath := m.dataPath(app, name)
	temp := path + backupTempSuffix
	dataTemp := dataPath + backupTempSuffix
	_ = os.Remove(temp)
	_ = os.Remove(dataTemp)
	hasDatabase := m.storage.hasDatabase(app)
	if hasDatabase {
		if err := m.storage.backupTo(ctx, app, temp); err != nil {
			_ = os.Remove(temp)
			return backupInfo{}, err
		}
	}
	if err := archiveData(filepath.Join(m.dataDir, app), dataTemp); err != nil {
		_ = os.Remove(temp)
		_ = os.Remove(dataTemp)
		return backupInfo{}, fmt.Errorf("archive data: %w", err)
	}
	candidateHash, err := snapshotHash(temp, dataTemp)
	if err != nil {
		log.Printf("backup %s: hash candidate: %v", app, err)
	}

	tree := state.Tree
	parentHash := ""
	if parent != "" {
		parentHash = m.parentSnapshotHash(app, parent, &tree)
	}
	if candidateHash != "" && candidateHash == parentHash {
		_ = os.Remove(temp)
		_ = os.Remove(dataTemp)
		tree.Checked = now
		if err := m.saveTreeLocked(app, tree); err != nil {
			log.Printf("backup %s: save tree: %v", app, err)
		}
		return backupInfo{}, errBackupUnchanged
	}

	if hasDatabase {
		if err := os.Rename(temp, path); err != nil {
			_ = os.Remove(temp)
			_ = os.Remove(dataTemp)
			return backupInfo{}, fmt.Errorf("install backup: %w", err)
		}
	}
	if err := os.Rename(dataTemp, dataPath); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(dataTemp)
		return backupInfo{}, fmt.Errorf("install data backup: %w", err)
	}
	var size int64
	if hasDatabase {
		info, err := os.Stat(path)
		if err != nil {
			return backupInfo{}, err
		}
		size = info.Size()
	}
	dataInfo, err := os.Stat(dataPath)
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
	return backupInfo{Name: name, Size: size, DataSize: dataInfo.Size(), HasData: true, CreatedAt: now, Parent: parent, Kind: kind}, nil
}
