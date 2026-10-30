package main

import (
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	deploymentKeep       = 10
	deploymentTimeLayout = "20060102T150405.000000000Z"
	deploymentMetaName   = "meta.json"
	deploymentBlobDir    = ".blobs"
)

// deploymentMaxBytes bounds one app's retained history. Identical artifacts are shared, so this is generous.
var deploymentMaxBytes int64 = 1 << 30

var errDeploymentNotFound = errors.New("deployment not found")

// deploymentInfo describes one retained bundle.
type deploymentInfo struct {
	ID          string            `json:"id"`
	DeployedAt  time.Time         `json:"deployedAt"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Runtime     string            `json:"runtime,omitempty"`
	Target      string            `json:"target,omitempty"`
	Rebuildable bool              `json:"rebuildable"`
	Bytes       int64             `json:"bytes"`
	Blobs       map[string]string `json:"blobs,omitempty"`
}

// deploymentManager keeps a bounded history of app bundles under the state directory.
type deploymentManager struct {
	dir string
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

func validDeploymentID(id string) error {
	if id == "" || filepath.Base(id) != id {
		return errDeploymentNotFound
	}
	if _, err := time.Parse(deploymentTimeLayout, id); err != nil {
		return errDeploymentNotFound
	}
	return nil
}

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

func (m *deploymentManager) bundle(app, id string) (string, error) {
	if err := validDeploymentID(id); err != nil {
		return "", err
	}
	dir := filepath.Join(m.appDir(app), id)
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

// storeBlob puts a file in the content-addressed store and returns its digest and size.
func (m *deploymentManager) storeBlob(app, source string) (string, int64, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", 0, err
	}
	digest, err := fileDigest(source)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(m.blobDir(app), 0o755); err != nil {
		return "", 0, err
	}
	blob := filepath.Join(m.blobDir(app), digest)
	if existing, err := os.Stat(blob); err == nil && existing.Size() == info.Size() {
		return digest, info.Size(), nil
	}

	// Copy rather than hardlink the source: a later in-place write to the live app must not mutate retained history.
	temp, err := os.CreateTemp(m.blobDir(app), ".blob-")
	if err != nil {
		return "", 0, err
	}
	name := temp.Name()
	src, err := os.Open(source)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(name)
		return "", 0, err
	}
	_, copyErr := io.Copy(temp, src)
	closeErr := src.Close()
	if copyErr == nil {
		copyErr = temp.Close()
	} else {
		_ = temp.Close()
	}
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(name)
		return "", 0, copyErr
	}
	if err := os.Rename(name, blob); err != nil {
		_ = os.Remove(name)
		return "", 0, err
	}
	return digest, info.Size(), nil
}

func linkBlob(blob, dst string) error {
	if err := os.Link(blob, dst); err == nil {
		return nil
	}
	return copyFile(blob, dst)
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
	}

	entries, err := os.ReadDir(m.blobDir(app))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := referenced[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(m.blobDir(app), entry.Name())); err != nil {
			return err
		}
	}
	return nil
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
	for len(ids) > deploymentKeep {
		if err := os.RemoveAll(filepath.Join(m.appDir(app), ids[0])); err != nil {
			return err
		}
		ids = ids[1:]
	}
	for len(ids) > 1 {
		used, err := m.usage(app)
		if err != nil {
			return err
		}
		if used <= deploymentMaxBytes {
			break
		}
		if err := os.RemoveAll(filepath.Join(m.appDir(app), ids[0])); err != nil {
			return err
		}
		ids = ids[1:]
		if err := m.gcBlobs(app); err != nil {
			return err
		}
	}
	return m.gcBlobs(app)
}
