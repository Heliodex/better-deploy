package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"
)

func FuzzValidateStorageSQL(f *testing.F) {
	for _, seed := range []string{
		`select 1`,
		`insert into notes (id, body) values (?, ?)`,
		`pragma user_version = 1`,
		`attach database '/etc/passwd' as other`,
		`select load_extension('x')`,
		`begin`,
		`select "pragma" from notes`,
		`select 'attach' from notes`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		_ = validateStorageSQL(query)
	})
}

func FuzzDecodeStorageValue(f *testing.F) {
	for _, seed := range []string{
		`null`, `true`, `false`, `"text"`, `42`, `-1.5e3`,
		`{"$b64":"AAEC"}`, `{"other":"value"}`, `{`, `[]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = decodeStorageValue(jsontext.Value(value))
	})
}

func fuzzTarGz(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name, body string
	}{
		{"app.json", `{"name":"demo"}`},
		{"main.wasm", "wasm"},
		{"../escape", "nope"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func FuzzExtractTarGz(f *testing.F) {
	f.Add(fuzzTarGz(f))
	f.Add([]byte("not gzip"))
	f.Fuzz(func(t *testing.T, data []byte) {
		src, err := os.CreateTemp(t.TempDir(), "archive-*.tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := src.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
		dst := t.TempDir()
		// Extraction may reject arbitrary bytes, but it must not panic or write outside dst.
		_ = extractTarGzLimited(src.Name(), dst, 1<<20, 100)
		if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); err == nil {
			t.Error("archive entry escaped the destination")
		}
	})
}
