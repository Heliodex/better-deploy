package main

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// acceptsEncoding reports whether an Accept-Encoding value permits one coding, including q=0 rejection.
func acceptsEncoding(header, encoding string) bool {
	for _, item := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(item), ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if name != encoding && name != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = parsed
				}
			}
		}
		return quality > 0
	}
	return false
}

// staticVariant picks a precompressed .br or .gz sidecar when the client accepts it. The original file must exist so a stray compressed file is never exposed on its own.
func staticVariant(dir, requestPath, acceptEncoding string) (path, original, encoding string, hasVariants, ok bool) {
	original, ok = staticFile(dir, requestPath)
	if !ok {
		return "", "", "", false, false
	}
	for _, sidecar := range []struct {
		encoding, suffix string
	}{{"br", ".br"}, {"gzip", ".gz"}} {
		candidate := original + sidecar.suffix
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			hasVariants = true
			if encoding == "" && acceptsEncoding(acceptEncoding, sidecar.encoding) {
				path, encoding = candidate, sidecar.encoding
			}
		}
	}
	if path == "" {
		path = original
	}
	return path, original, encoding, hasVariants, true
}

func serveStaticFile(w http.ResponseWriter, r *http.Request, path, original, requestPath, encoding string, hasVariants bool) {
	if strings.HasPrefix(requestPath, "/_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if hasVariants {
		addVary(w.Header(), "Accept-Encoding")
	}
	if encoding != "" {
		w.Header().Set("Content-Encoding", encoding)
		contentType := mime.TypeByExtension(filepath.Ext(original))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
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
	if path, original, encoding, hasVariants, ok := staticVariant(dir, r.URL.Path, r.Header.Get("Accept-Encoding")); ok {
		serveStaticFile(w, r, path, original, r.URL.Path, encoding, hasVariants)
		return true
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		return false
	}
	if path, original, encoding, hasVariants, ok := staticVariant(dir, "/index.html", r.Header.Get("Accept-Encoding")); ok {
		serveStaticFile(w, r, path, original, "/", encoding, hasVariants)
		return true
	}
	return false
}
