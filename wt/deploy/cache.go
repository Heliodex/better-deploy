package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const buildCacheName = ".wt-build.json"

// buildCache records the component-input hash of the last successful build so a static-only edit can skip componentize-go.
type buildCache struct {
	ComponentHash string `json:"componentHash"`
	WasmDigest    string `json:"wasmDigest,omitempty"`
}

var buildIgnoredDirs = map[string]struct{}{
	".git":             {},
	"node_modules":     {},
	"web/build":        {},
	"web/node_modules": {},
	"web/.svelte-kit":  {},
	"static":           {},
	"dist":             {},
	"target":           {},
	"sdk/binding":      {},
	"sdk/tmp":          {},
	"contract/schemas": {},
	"tmp":              {},
}

var buildIgnoredFiles = map[string]struct{}{
	"main.wasm":     {},
	"main.cwasm":    {},
	"artifact.json": {},
	buildCacheName:  {},
}

// componentInputHash hashes the files that can change the component, excluding static output, generated bindings and build caches.
func componentInputHash(dir, build string) (string, error) {
	hash := sha256.New()
	_, _ = io.WriteString(hash, "build\x00"+build+"\x00")
	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			if _, skip := buildIgnoredDirs[rel]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if _, skip := buildIgnoredFiles[rel]; skip {
			return nil
		}
		if strings.HasSuffix(rel, ".tar.gz") || strings.HasSuffix(rel, ".wt-build.json.tmp") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hash, rel+"\x00")
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readBuildCache(dir string) (buildCache, error) {
	var cache buildCache
	data, err := os.ReadFile(filepath.Join(dir, buildCacheName))
	if os.IsNotExist(err) {
		return cache, nil
	}
	if err != nil {
		return cache, err
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return buildCache{}, nil
	}
	return cache, nil
}

func writeBuildCache(dir, componentHash, wasmDigest string) {
	data, err := json.Marshal(buildCache{ComponentHash: componentHash, WasmDigest: wasmDigest})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, buildCacheName), append(data, '\n'), 0o644)
}

// prepareComponent skips the build and compile when the component inputs are unchanged and main.wasm is present.
func prepareComponent(cfg *deployConfig, m manifest) (bool, error) {
	if cfg.skipBuild {
		if err := runBuild(cfg, m); err != nil {
			return false, err
		}
		if err := runCompile(cfg); err != nil {
			return false, err
		}
		return false, nil
	}
	hash, err := componentInputHash(cfg.dir, m.Build)
	if err != nil {
		return false, err
	}
	cache, _ := readBuildCache(cfg.dir)
	if !cfg.forceBuild && cache.ComponentHash == hash {
		if info, err := os.Stat(filepath.Join(cfg.dir, "main.wasm")); err == nil && info.Mode().IsRegular() {
			return true, nil
		}
	}
	if err := runBuild(cfg, m); err != nil {
		return false, err
	}
	if err := runCompile(cfg); err != nil {
		return false, err
	}
	digest, _ := fileDigest(filepath.Join(cfg.dir, "main.wasm"))
	writeBuildCache(cfg.dir, hash, digest)
	return false, nil
}

// bundleResult carries the archive and whether it omitted the component for a static-only deploy.
type bundleResult struct {
	manifest manifest
	data     []byte
	reused   bool
}

// serverDigest returns the currently running component digest, or an empty string when the app does not exist.
func serverDigest(cfg *deployConfig, name string) string {
	req, err := httpRequest(cfg, "GET", "/apps/"+name, nil)
	if err != nil {
		return ""
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var info struct {
		CurrentDigest string `json:"currentDigest"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return ""
	}
	return info.CurrentDigest
}

func httpRequest(cfg *deployConfig, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, baseURL(cfg.host)+path, body)
	if err != nil {
		return nil, err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	return req, nil
}
