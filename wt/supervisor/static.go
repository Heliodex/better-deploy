package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// staticDir is the bundle's static/ directory, served by the supervisor instead of the component.
func (a *app) staticDir() string {
	a.mu.Lock()
	dir := a.dir
	a.mu.Unlock()
	return filepath.Join(dir, "static")
}

// staticFile maps a request path to a regular file inside dir, rejecting traversal and directories.
func staticFile(dir, requestPath string) (string, bool) {
	clean := filepath.Clean("/" + requestPath)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	path := filepath.Join(dir, filepath.FromSlash(rel))
	inside, err := filepath.Rel(dir, path)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

func serveStaticFile(w http.ResponseWriter, r *http.Request, path, requestPath string) {
	if strings.HasPrefix(requestPath, "/_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, path)
}

// serveStatic answers a request from the bundle's static/ directory. Unknown paths fall back to index.html only for HTML navigations, so API routes still reach the component.
func (a *app) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	dir := a.staticDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}
	if path, ok := staticFile(dir, r.URL.Path); ok {
		serveStaticFile(w, r, path, r.URL.Path)
		return true
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		return false
	}
	index := filepath.Join(dir, "index.html")
	if info, err := os.Stat(index); err != nil || !info.Mode().IsRegular() {
		return false
	}
	serveStaticFile(w, r, index, "/")
	return true
}
