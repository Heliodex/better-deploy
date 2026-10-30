package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
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
	if err := os.Link(source, blob); err == nil {
		return digest, info.Size(), nil
	}

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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// copyOptional copies a file when it exists, leaving legacy bundles without main.wasm or artifact.json valid.
func copyOptional(src, dst, name string) error {
	if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("copy %s: %w", name, err)
	}
	return nil
}

func copyBundle(src, dst string) error {
	if err := copyFile(filepath.Join(src, "app.json"), filepath.Join(dst, "app.json")); err != nil {
		return fmt.Errorf("copy app.json: %w", err)
	}
	for _, name := range []string{"main.cwasm", "main.wasm", artifactName} {
		if err := copyOptional(src, dst, name); err != nil {
			return err
		}
	}
	return nil
}

func fileDigest(path string) (string, error) {
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

func (m *deploymentManager) record(app, source string) (deploymentInfo, error) {
	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return deploymentInfo{}, fmt.Errorf("deployment dir: %w", err)
	}
	now := time.Now().UTC()
	id := now.Format(deploymentTimeLayout)
	for exists(m.appDir(app), id) {
		now = now.Add(time.Nanosecond)
		id = now.Format(deploymentTimeLayout)
	}

	temp, err := os.MkdirTemp(m.appDir(app), ".record-")
	if err != nil {
		return deploymentInfo{}, err
	}
	defer os.RemoveAll(temp)

	artifact, _ := readArtifact(source)
	files := map[string]string{}
	var total, cwasmSize, wasmSize int64
	digest := ""
	for _, name := range []string{"main.cwasm", "main.wasm"} {
		info, err := os.Stat(filepath.Join(source, name))
		if err != nil {
			continue
		}
		stored, size, err := m.storeBlob(app, filepath.Join(source, name))
		if err != nil {
			return deploymentInfo{}, fmt.Errorf("store %s: %w", name, err)
		}
		if err := linkBlob(filepath.Join(m.blobDir(app), stored), filepath.Join(temp, name)); err != nil {
			return deploymentInfo{}, fmt.Errorf("link %s: %w", name, err)
		}
		files[name] = stored
		total += size
		switch name {
		case "main.cwasm":
			digest, cwasmSize = stored, info.Size()
		case "main.wasm":
			wasmSize = info.Size()
		}
	}
	if digest == "" {
		digest = files["main.wasm"]
		cwasmSize = wasmSize
	}
	if digest == "" {
		return deploymentInfo{}, errNoComponent
	}
	if err := copyFile(filepath.Join(source, "app.json"), filepath.Join(temp, "app.json")); err != nil {
		return deploymentInfo{}, err
	}
	if err := copyOptional(source, temp, artifactName); err != nil {
		return deploymentInfo{}, err
	}
	for _, name := range []string{"app.json", artifactName} {
		if info, err := os.Stat(filepath.Join(temp, name)); err == nil {
			total += info.Size()
		}
	}
	recorded := deploymentInfo{
		ID:          id,
		DeployedAt:  now,
		Digest:      digest,
		Size:        cwasmSize,
		Runtime:     artifact.Wasmtime,
		Target:      artifact.Target,
		Rebuildable: files["main.wasm"] != "",
		Bytes:       total,
		Blobs:       files,
	}
	meta, err := json.Marshal(recorded)
	if err != nil {
		return deploymentInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(temp, deploymentMetaName), append(meta, '\n'), 0o644); err != nil {
		return deploymentInfo{}, err
	}
	if err := os.Rename(temp, filepath.Join(m.appDir(app), id)); err != nil {
		return deploymentInfo{}, err
	}
	if err := m.prune(app); err != nil {
		log.Printf("deploy history %s: prune: %v", app, err)
	}
	return recorded, nil
}

// historyBytes returns cached on-disk deployment history size, counting shared artifacts once.
func (s *supervisor) historyBytes(a *app) int64 {
	if s.deployments == nil {
		return 0
	}
	now := time.Now()
	a.history.mu.Lock()
	if !a.history.at.IsZero() && now.Sub(a.history.at) < diskUsageInterval {
		bytes := a.history.bytes
		a.history.mu.Unlock()
		return bytes
	}
	a.history.mu.Unlock()

	bytes, err := s.deployments.usage(a.name)
	if err != nil {
		log.Printf("app %s: deployment history usage: %v", a.name, err)
	}
	a.history.mu.Lock()
	a.history.bytes = bytes
	a.history.at = now
	a.history.mu.Unlock()
	return bytes
}

func (s *supervisor) recordDeployment(name, dir string) {
	if s.deployments == nil {
		return
	}
	if _, err := s.deployments.record(name, dir); err != nil {
		log.Printf("deploy history %s: %v", name, err)
	}
	if a := s.lookup(name); a != nil {
		a.history.mu.Lock()
		a.history.at = time.Time{}
		a.history.mu.Unlock()
	}
}

func (s *supervisor) handleDeployments(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}
	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}
	list, err := s.deployments.list(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *supervisor) handleRollback(w http.ResponseWriter, r *http.Request, name, id string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if _, ok := s.requireApp(w, r, name); !ok {
		return
	}
	if s.deployments == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history unavailable")
		return
	}
	bundle, err := s.deployments.bundle(name, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	dir, err := os.MkdirTemp(s.cfg.appsDir, "."+name+".rollback-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create rollback directory")
		return
	}
	defer os.RemoveAll(dir)
	if err := copyBundle(bundle, dir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, err := readAppConfig(dir, name, s.cfg.domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Name = name
	if s.hostTaken(cfg.Host, name) {
		writeError(w, http.StatusConflict, fmt.Sprintf("host %q is already used by another app", cfg.Host))
		return
	}
	next := s.installBundle(w, name, cfg, dir, "rolled back")
	if next == nil {
		return
	}
	s.recordDeployment(name, next.dir)
	writeJSON(w, http.StatusOK, s.info(next))
}
