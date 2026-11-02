package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// contentStore writes immutable files named by their SHA-256 digest.
// The directory holds only content-addressed files; metadata and retention live with the caller.
type contentStore struct {
	dir, tempDir string
}

func newContentStore(dir, tempDir string) *contentStore {
	return &contentStore{dir: dir, tempDir: tempDir}
}

// path returns the location of one digest.
func (c *contentStore) path(digest string) string {
	return filepath.Join(c.dir, digest)
}

// putFile copies source into the store unless an equally sized blob already exists.
func (c *contentStore) putFile(source string) (string, int64, bool, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", 0, false, err
	}
	digest, err := fileDigest(source)
	if err != nil {
		return "", 0, false, err
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", 0, false, err
	}
	blob := c.path(digest)
	if existing, err := os.Stat(blob); err == nil && existing.Size() == info.Size() {
		return digest, info.Size(), false, nil
	}
	temp, err := os.CreateTemp(c.dir, ".blob-")
	if err != nil {
		return "", 0, false, err
	}
	name := temp.Name()
	src, err := os.Open(source)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(name)
		return "", 0, false, err
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
		return "", 0, false, copyErr
	}
	if err := os.Rename(name, blob); err != nil {
		_ = os.Remove(name)
		return "", 0, false, err
	}
	return digest, info.Size(), true, nil
}

// putReader streams src into the store and reports its digest, size and whether it was new.
// It reads at most maxBytes+1 to distinguish an oversized body without buffering it.
func (c *contentStore) putReader(src io.Reader, maxBytes int64) (digest string, size int64, created, tooLarge bool, err error) {
	if err := os.MkdirAll(c.tempDir, 0o700); err != nil {
		return "", 0, false, false, err
	}
	temp, err := os.CreateTemp(c.tempDir, "upload-")
	if err != nil {
		return "", 0, false, false, err
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(src, maxBytes+1))
	closeErr := temp.Close()
	if err != nil {
		cleanup()
		return "", 0, false, false, err
	}
	if closeErr != nil {
		cleanup()
		return "", 0, false, false, closeErr
	}
	if written > maxBytes {
		cleanup()
		return "", 0, false, true, nil
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	target := c.path(digest)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		cleanup()
		return digest, written, false, false, nil
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		cleanup()
		return "", 0, false, false, err
	}
	if err := os.Rename(name, target); err != nil {
		cleanup()
		return "", 0, false, false, err
	}
	return digest, written, true, false, nil
}

// link hardlinks a stored blob into dst, falling back to a copy across filesystems.
func (c *contentStore) link(digest, dst string) error {
	if err := os.Link(c.path(digest), dst); err == nil {
		return nil
	}
	return copyFile(c.path(digest), dst)
}

// prune removes stored files that the retained metadata no longer references.
func (c *contentStore) prune(referenced map[string]struct{}) error {
	entries, err := os.ReadDir(c.dir)
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
		if err := os.Remove(filepath.Join(c.dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
