// Recording, copying and serving retained deployment bundles.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

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

// copyStatic copies the bundle's static/ directory when it exists.
func copyStatic(src, dst string) error {
	root := filepath.Join(src, "static")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target)
	})
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
	return copyStatic(src, dst)
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
	var cwasmDigest, wasmDigest string
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
			cwasmDigest, cwasmSize = stored, info.Size()
		case "main.wasm":
			wasmDigest, wasmSize = stored, info.Size()
		}
	}
	digest, size := wasmDigest, wasmSize
	if digest == "" {
		digest, size = cwasmDigest, cwasmSize
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
	if err := copyStatic(source, temp); err != nil {
		return deploymentInfo{}, err
	}
	for _, name := range []string{"app.json", artifactName} {
		if info, err := os.Stat(filepath.Join(temp, name)); err == nil {
			total += info.Size()
		}
	}
	static := filepath.Join(temp, "static")
	if info, err := os.Stat(static); err == nil && info.IsDir() {
		if err := walkSize(static, &total); err != nil {
			return deploymentInfo{}, err
		}
	}
	recorded := deploymentInfo{
		ID:          id,
		DeployedAt:  now,
		Digest:      digest,
		Size:        size,
		WasmDigest:  wasmDigest,
		WasmBytes:   wasmSize,
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
	// ?backup=<snapshot> swaps code, database and /data as one operation, with compensation on failure.
	if backup := r.URL.Query().Get("backup"); backup != "" {
		if s.backups == nil {
			writeError(w, http.StatusServiceUnavailable, "backups unavailable")
			return
		}
		next := s.rollbackAtomic(w, r.Context(), name, dir, cfg, backup)
		if next == nil {
			return
		}
		writeJSON(w, http.StatusOK, s.info(next))
		return
	}
	next, mode := s.installBundle(w, name, cfg, dir, "rolled back")
	if next == nil {
		return
	}
	s.recordDeployment(name, next.dir)
	w.Header().Set("X-WT-Wasm-Digest", next.digest)
	w.Header().Set("X-WT-Deploy-Mode", mode)
	writeJSON(w, http.StatusOK, s.info(next))
}
