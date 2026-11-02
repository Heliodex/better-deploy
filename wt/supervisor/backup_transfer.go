// Import and export of backup snapshots as portable archives.

package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeTarFile copies one file into an export archive under name.
func writeTarFile(tw *tar.Writer, name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.CopyN(tw, file, info.Size())
	return err
}

// exportSnapshot packs a snapshot's database and data archive into a portable tar.gz and returns its path.
func (m *backupManager) exportSnapshot(app, name string) (string, error) {
	name, err := validBackupName(name)
	if err != nil {
		return "", err
	}
	m.lock(app)
	defer m.unlock(app)

	database := filepath.Join(m.appDir(app), name)
	archive := m.dataPath(app, name)
	databaseInfo, databaseErr := os.Stat(database)
	archiveInfo, archiveErr := os.Stat(archive)
	hasDatabase := databaseErr == nil && databaseInfo.Mode().IsRegular()
	hasData := archiveErr == nil && archiveInfo.Mode().IsRegular()
	if !hasDatabase && !hasData {
		return "", errBackupNotFound
	}

	temp, err := os.CreateTemp(m.dataDir, "."+app+".export-*.tar.gz")
	if err != nil {
		return "", err
	}
	output := temp.Name()
	gz := gzip.NewWriter(temp)
	tw := tar.NewWriter(gz)
	writeErr := error(nil)
	if hasDatabase {
		writeErr = writeTarFile(tw, name, database)
	}
	if writeErr == nil && hasData {
		writeErr = writeTarFile(tw, filepath.Base(archive), archive)
	}
	if writeErr == nil {
		writeErr = tw.Close()
	} else {
		_ = tw.Close()
	}
	if writeErr == nil {
		writeErr = gz.Close()
	} else {
		_ = gz.Close()
	}
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(output)
		return "", writeErr
	}
	return output, nil
}

// extractSnapshot unpacks an import archive and returns its database and data archive paths.
func extractSnapshot(src, temp string) (database, archive string, err error) {
	if err := extractTarGzLimited(src, temp, maxBackupExtractBytes, maxBackupTarFiles); err != nil {
		return "", "", err
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		return "", "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(temp, entry.Name())
		switch {
		case strings.HasSuffix(entry.Name(), backupFileSuffix):
			if database != "" {
				return "", "", fmt.Errorf("snapshot contains more than one database")
			}
			database = path
		case strings.HasSuffix(entry.Name(), backupDataSuffix):
			if archive != "" {
				return "", "", fmt.Errorf("snapshot contains more than one data archive")
			}
			archive = path
		}
	}
	if database == "" && archive == "" {
		return "", "", fmt.Errorf("snapshot has no database or data archive")
	}
	if database != "" {
		if err := checkBackupDatabase(database); err != nil {
			return "", "", fmt.Errorf("snapshot database: %w", err)
		}
	}
	return database, archive, nil
}

// placeImportedSnapshot moves the extracted database and data archive into the backup directory.
func (m *backupManager) placeImportedSnapshot(app, name, database, archive string) (path, dataPath string, err error) {
	path = filepath.Join(m.appDir(app), name)
	dataPath = m.dataPath(app, name)
	if database != "" {
		if err := m.adoptBackupFile(app, database, path); err != nil {
			return "", "", err
		}
	}
	if archive != "" {
		if err := m.adoptBackupFile(app, archive, dataPath); err != nil {
			_ = os.Remove(path)
			return "", "", err
		}
	}
	return path, dataPath, nil
}

// recordImportedSnapshot adds the imported snapshot to the backup tree and reports its size.
func (m *backupManager) recordImportedSnapshot(app, name string, now time.Time, path, dataPath string) (backupInfo, error) {
	state, err := m.stateLocked(app)
	if err != nil {
		_ = os.Remove(path)
		_ = os.Remove(dataPath)
		return backupInfo{}, err
	}
	hash, err := snapshotHash(path, dataPath)
	if err != nil {
		log.Printf("backup %s: hash import: %v", app, err)
	}
	tree := state.Tree
	tree.Nodes[name] = backupNode{Parent: tree.Current, Kind: backupKindImport, Hash: hash}
	if err := m.saveTreeLocked(app, tree); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(dataPath)
		return backupInfo{}, err
	}
	// Keep the imported snapshot even when the app's keep limit is already full; the next snapshot or settings change prunes it.
	info := backupInfo{Name: name, CreatedAt: now, Parent: tree.Nodes[name].Parent, Kind: backupKindImport}
	if infoD, err := os.Stat(path); err == nil {
		info.Size = infoD.Size()
	}
	if infoD, err := os.Stat(dataPath); err == nil {
		info.DataSize = infoD.Size()
		info.HasData = true
	}
	return info, nil
}

// importSnapshot extracts an uploaded archive into a temporary directory and records it as a new snapshot.
func (m *backupManager) importSnapshot(app, src string) (backupInfo, error) {
	m.lock(app)
	defer m.unlock(app)

	temp, err := os.MkdirTemp(m.dataDir, "."+app+".import-")
	if err != nil {
		return backupInfo{}, err
	}
	defer os.RemoveAll(temp)
	database, archive, err := extractSnapshot(src, temp)
	if err != nil {
		return backupInfo{}, err
	}
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return backupInfo{}, err
	}
	now := time.Now().UTC()
	name, now := m.backupName(app, now)
	path, dataPath, err := m.placeImportedSnapshot(app, name, database, archive)
	if err != nil {
		return backupInfo{}, err
	}
	return m.recordImportedSnapshot(app, name, now, path, dataPath)
}
