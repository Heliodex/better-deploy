package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readArchiveEntries returns each tar entry name and its contents from a gzip-compressed archive.
func readArchiveEntries(t *testing.T, path string) map[string]string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}

	entries := map[string]string{}
	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}

		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		entries[header.Name] = string(data)
	}
}

func TestArchiveDataWritesRegularFilesAndDirectories(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "root.txt"), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "nested", "inner.txt"), []byte("inner"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink is not a regular file, so the archive must skip it rather than follow it.
	if err := os.Symlink(filepath.Join(src, "root.txt"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "data.tar.gz")
	if err := archiveData(src, dst); err != nil {
		t.Fatalf("archiveData() error = %v", err)
	}

	entries := readArchiveEntries(t, dst)
	if entries["root.txt"] != "root" || entries["nested/inner.txt"] != "inner" {
		t.Fatalf("archive entries = %v, want root.txt and nested/inner.txt", entries)
	}

	if _, ok := entries["nested/"]; !ok {
		t.Fatalf("archive entries = %v, want a nested/ directory entry", entries)
	}

	if _, ok := entries["link"]; ok {
		t.Fatalf("archive entries = %v, want the symlink skipped", entries)
	}
}

func TestArchiveDataMissingSourceIsEmpty(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "data.tar.gz")
	if err := archiveData(filepath.Join(t.TempDir(), "absent"), dst); err != nil {
		t.Fatalf("archiveData() error = %v", err)
	}

	if entries := readArchiveEntries(t, dst); len(entries) != 0 {
		t.Fatalf("archive entries = %v, want none", entries)
	}
}
