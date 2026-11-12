package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// copyOptional copies a file when it exists, leaving legacy bundles without main.wasm or artefact.json valid.
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

// copyBundle copies the deployable files and static output from a bundle directory.
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

// storeStatic content-addresses every regular file under source/static and hardlinks it into dst/static at the same relative path. It returns the path-to-digest map and the unique bytes stored.
func (m *deploymentManager) storeStatic(app, source, dst string) (map[string]string, int64, error) {
	root := filepath.Join(source, "static")

	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}

	if err != nil {
		return nil, 0, err
	}

	if !info.IsDir() {
		return nil, 0, nil
	}

	files := map[string]string{}
	seen := map[string]struct{}{}

	var total int64

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		stored, size, err := m.storeBlob(app, path)
		if err != nil {
			return fmt.Errorf("store static %s: %w", rel, err)
		}

		target := filepath.Join(dst, "static", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		if err := m.content(app).link(stored, target); err != nil {
			return fmt.Errorf("link static %s: %w", rel, err)
		}

		files[filepath.ToSlash(rel)] = stored
		if _, ok := seen[stored]; !ok {
			seen[stored] = struct{}{}
			total += size
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return files, total, nil
}

// componentRecord describes the component files stored for one deployment.
type componentRecord struct {
	files                   map[string]string
	cwasmDigest, wasmDigest string
	cwasmSize, wasmSize     int64
	bytes                   int64
}

// digest returns the preferred artifact digest, preferring the portable wasm over the cwasm.
func (r componentRecord) digest() (digest string, size int64, ok bool) {
	if r.wasmDigest != "" {
		return r.wasmDigest, r.wasmSize, true
	}

	if r.cwasmDigest != "" {
		return r.cwasmDigest, r.cwasmSize, true
	}

	return "", 0, false
}

// storeComponent stores and links main.cwasm and main.wasm into the deployment directory.
func (m *deploymentManager) storeComponent(app, source, temp string) (componentRecord, error) {
	record := componentRecord{files: map[string]string{}}

	for _, name := range []string{"main.cwasm", "main.wasm"} {
		info, err := os.Stat(filepath.Join(source, name))
		if err != nil {
			continue
		}

		stored, size, err := m.storeBlob(app, filepath.Join(source, name))
		if err != nil {
			return record, fmt.Errorf("store %s: %w", name, err)
		}

		if err := m.content(app).link(stored, filepath.Join(temp, name)); err != nil {
			return record, fmt.Errorf("link %s: %w", name, err)
		}

		record.files[name] = stored
		record.bytes += size

		switch name {
		case "main.cwasm":
			record.cwasmDigest, record.cwasmSize = stored, info.Size()
		case "main.wasm":
			record.wasmDigest, record.wasmSize = stored, info.Size()
		}
	}

	return record, nil
}

// stageDeployment copies the manifest, artifact metadata, static files and previous log into the staging directory.
func (m *deploymentManager) stageDeployment(app, id, source, temp string) (componentRecord, map[string]string, int64, error) {
	record, err := m.storeComponent(app, source, temp)
	if err != nil {
		return record, nil, 0, err
	}

	if _, _, ok := record.digest(); !ok {
		return record, nil, 0, errNoComponent
	}

	if err := copyFile(filepath.Join(source, "app.json"), filepath.Join(temp, "app.json")); err != nil {
		return record, nil, 0, err
	}

	if err := copyOptional(source, temp, artifactName); err != nil {
		return record, nil, 0, err
	}

	static, staticBytes, err := m.storeStatic(app, source, temp)
	if err != nil {
		return record, nil, 0, err
	}

	total := record.bytes + staticBytes

	for _, name := range []string{"app.json", artifactName} {
		if info, err := os.Stat(filepath.Join(temp, name)); err == nil {
			total += info.Size()
		}
	}

	if err := copyOptional(filepath.Join(m.appDir(app), id), temp, deploymentLogName); err != nil {
		return record, nil, 0, err
	}

	return record, static, total, nil
}

// writeDeploymentMeta writes the entry beside the staged bundle.
func writeDeploymentMeta(temp string, info deploymentInfo) error {
	meta, err := json.Marshal(info)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(temp, deploymentMetaName), append(meta, '\n'), 0o644); err != nil {
		return err
	}

	return nil
}
