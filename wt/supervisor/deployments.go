package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	deploymentKeep       = 10
	deploymentTimeLayout = "20060102T150405.000000000Z"
	deploymentMetaName   = "meta.json"
)

var errDeploymentNotFound = errors.New("deployment not found")

// deploymentInfo describes one retained bundle.
type deploymentInfo struct {
	ID          string    `json:"id"`
	DeployedAt  time.Time `json:"deployedAt"`
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	Runtime     string    `json:"runtime,omitempty"`
	Target      string    `json:"target,omitempty"`
	Rebuildable bool      `json:"rebuildable"`
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
	return nil
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
	cwasm := filepath.Join(source, "main.cwasm")
	info, err := os.Stat(cwasm)
	rebuildable := false
	if _, err := os.Stat(filepath.Join(source, "main.wasm")); err == nil {
		rebuildable = true
		if info == nil {
			info, err = os.Stat(filepath.Join(source, "main.wasm"))
		}
	}
	if err != nil {
		return deploymentInfo{}, fmt.Errorf("read main.cwasm: %w", err)
	}
	digest, err := fileDigest(cwasm)
	if os.IsNotExist(err) {
		digest, err = fileDigest(filepath.Join(source, "main.wasm"))
	}
	if err != nil {
		return deploymentInfo{}, err
	}
	if err := copyBundle(source, temp); err != nil {
		return deploymentInfo{}, err
	}
	recorded := deploymentInfo{
		ID:          id,
		DeployedAt:  now,
		Digest:      digest,
		Size:        info.Size(),
		Runtime:     artifact.Wasmtime,
		Target:      artifact.Target,
		Rebuildable: rebuildable,
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

func (s *supervisor) recordDeployment(name, dir string) {
	if s.deployments == nil {
		return
	}
	if _, err := s.deployments.record(name, dir); err != nil {
		log.Printf("deploy history %s: %v", name, err)
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
