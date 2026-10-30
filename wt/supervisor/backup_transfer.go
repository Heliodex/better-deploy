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
	m.mu.Lock()
	defer m.mu.Unlock()

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

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// importSnapshot stores a snapshot from an export archive. It becomes a child of the current snapshot but does not become current until restored.
func (m *backupManager) importSnapshot(app, src string) (backupInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	temp, err := os.MkdirTemp(m.dataDir, "."+app+".import-")
	if err != nil {
		return backupInfo{}, err
	}
	defer os.RemoveAll(temp)
	if err := extractTarGzLimited(src, temp, maxBackupExtractBytes, maxBackupTarFiles); err != nil {
		return backupInfo{}, err
	}

	database := ""
	archive := ""
	entries, err := os.ReadDir(temp)
	if err != nil {
		return backupInfo{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(temp, entry.Name())
		switch {
		case strings.HasSuffix(entry.Name(), backupFileSuffix):
			if database != "" {
				return backupInfo{}, fmt.Errorf("snapshot contains more than one database")
			}
			database = path
		case strings.HasSuffix(entry.Name(), backupDataSuffix):
			if archive != "" {
				return backupInfo{}, fmt.Errorf("snapshot contains more than one data archive")
			}
			archive = path
		}
	}
	if database == "" && archive == "" {
		return backupInfo{}, fmt.Errorf("snapshot has no database or data archive")
	}
	if database != "" {
		if err := checkBackupDatabase(database); err != nil {
			return backupInfo{}, fmt.Errorf("snapshot database: %w", err)
		}
	}

	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return backupInfo{}, err
	}
	now := time.Now().UTC()
	name, now := m.backupName(app, now)
	path := filepath.Join(m.appDir(app), name)
	dataPath := m.dataPath(app, name)
	if database != "" {
		if err := moveFile(database, path); err != nil {
			return backupInfo{}, err
		}
	}
	if archive != "" {
		if err := moveFile(archive, dataPath); err != nil {
			_ = os.Remove(path)
			return backupInfo{}, err
		}
	}

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
