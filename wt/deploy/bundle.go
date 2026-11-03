package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: tarFileMode, Size: int64(len(data))}); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	return nil
}

// writeTarPath adds a file from disk to the bundle with its relative name.
func writeTarPath(tw *tar.Writer, name, path string) error {
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

// bundle packs app.json, the component files and static output into a gzipped tarball for upload.
// manifestJSON preserves app.json fields the deploy tool does not model, while applying name and host overrides.
func manifestJSON(dir string, m manifest) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		return json.Marshal(m, jsontext.WithIndent("  "))
	}

	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}

	if m.Name != "" {
		values["name"] = m.Name
	}

	if m.Host != "" {
		values["host"] = m.Host
	}

	return json.Marshal(values, jsontext.WithIndent("  "))
}

func bundle(dir string, m manifest, includeComponent bool) ([]byte, error) {
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	appJSON, err := manifestJSON(dir, m)
	if err != nil {
		return nil, err
	}

	if err := writeTarFile(tw, "app.json", append(appJSON, '\n')); err != nil {
		return nil, err
	}

	if includeComponent {
		wasm, err := os.ReadFile(filepath.Join(dir, "main.wasm"))
		if err != nil {
			return nil, fmt.Errorf("read main.wasm: %w (did the build run?)", err)
		}

		if err := writeTarFile(tw, "main.wasm", wasm); err != nil {
			return nil, err
		}
		// main.cwasm is only a precompiled hint; the supervisor can compile from main.wasm or reuse its cache.
		for _, name := range []string{"main.cwasm", artifactName} {
			extra, err := os.ReadFile(filepath.Join(dir, name))
			if os.IsNotExist(err) {
				continue
			}

			if err != nil {
				return nil, fmt.Errorf("read %s: %w", name, err)
			}

			if err := writeTarFile(tw, name, extra); err != nil {
				return nil, err
			}
		}
	}
	// Static files are served by the supervisor, so a frontend change does not need to be inside the component.
	static := filepath.Join(dir, "static")
	if info, err := os.Stat(static); err == nil && info.IsDir() {
		err := filepath.WalkDir(static, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if entry.IsDir() {
				return nil
			}

			if !entry.Type().IsRegular() {
				return nil
			}

			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}

			return writeTarPath(tw, filepath.ToSlash(rel), path)
		})
		if err != nil {
			return nil, fmt.Errorf("add static files: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
