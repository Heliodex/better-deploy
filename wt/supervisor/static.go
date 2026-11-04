package main

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// staticPageHeader lets a component name a page in the bundle's static directory to serve instead of its own body. The app opts navigation prefixes into this path with server_pages.
const staticPageHeader = "X-Wt-Static-Page"

// staticPageRequestKey marks a navigation the component sees before a matching static file.
type staticPageRequestKey struct{}

// staticDir is the bundle's static/ directory, served by the supervisor instead of the component.
func (a *app) staticDir() string {
	a.mu.Lock()
	dir := a.dir
	a.mu.Unlock()

	return filepath.Join(dir, "static")
}

// staticRouting returns the static directory, the app's server-only prefixes and its server-selected page prefixes under one lock.
func (a *app) staticRouting() (string, []string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return filepath.Join(a.dir, "static"), append([]string(nil), a.cfg.ServerPaths...), append([]string(nil), a.cfg.ServerPages...)
}

// htmlNavigation reports whether a request is a browser page navigation rather than an asset or API call.
func htmlNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// serverPageNavigation reports whether a request belongs to a prefix where the component selects the page.
func (a *app) serverPageNavigation(r *http.Request) bool {
	if !htmlNavigation(r) {
		return false
	}

	a.mu.Lock()
	prefixes := append([]string(nil), a.cfg.ServerPages...)
	a.mu.Unlock()

	return serverPathMatch(r.URL.Path, prefixes)
}

// staticRegularFile reports path as a regular file inside dir.
func staticRegularFile(dir, path string) (string, bool) {
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

// staticFile maps a request path to a regular file inside dir, rejecting traversal, resolving a directory's index.html for a trailing-slash request, and trying <path>.html for extensionless prerendered pages.
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
	if err != nil {
		// Pages built with trailingSlash "never" are written as <route>.html.
		if os.IsNotExist(err) && filepath.Ext(rel) == "" {
			return staticRegularFile(dir, path+".html")
		}

		return "", false
	}

	if info.IsDir() {
		if !strings.HasSuffix(requestPath, "/") {
			return "", false
		}

		return staticRegularFile(dir, filepath.Join(path, "index.html"))
	}

	if !info.Mode().IsRegular() {
		return "", false
	}

	return path, true
}

// staticPageFile resolves a page path in dir, accepting both the trailing-slash directory index and the extensionless <route>.html form. Only HTML documents are pages.
func staticPageFile(dir, requestPath string) (string, bool) {
	candidates := []string{requestPath}

	switch {
	case requestPath == "" || requestPath == "/":
		candidates[0] = "/index.html"
	case !strings.HasSuffix(requestPath, "/"):
		candidates = append(candidates, requestPath+"/")
	}

	for _, candidate := range candidates {
		path, ok := staticFile(dir, candidate)
		if !ok || !isHTMLFile(path) {
			continue
		}

		return path, true
	}

	return "", false
}

// isHTMLFile reports whether a resolved static path is an HTML document.
func isHTMLFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return true
	}

	return false
}

// validStaticPagePath rejects values that name more than a path inside the static directory.
func validStaticPagePath(value string) bool {
	return value != "" && strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "?#\\\x00\t\r\n")
}

// serverPathMatch reports whether a request path belongs to a prefix the app handles at runtime instead of the SPA index.
func serverPathMatch(requestPath string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix == "/" || requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return true
		}
	}

	return false
}

// acceptsEncoding reports whether an Accept-Encoding value permits one coding, including q=0 rejection.
func acceptsEncoding(header, encoding string) bool {
	for item := range strings.SplitSeq(header, ",") {
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

// serveStaticFile writes the static caching headers, content encoding and type.
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

// notFoundStaticPage replaces a component response with an empty 404 after it named a page that does not exist.
func notFoundStaticPage(res *http.Response) {
	if res.Body != nil {
		_ = res.Body.Close()
	}

	res.StatusCode = http.StatusNotFound
	res.Status = "404 Not Found"
	res.Body = http.NoBody
	res.ContentLength = 0
	res.TransferEncoding = nil
	res.Trailer = nil
	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Length")
	res.Header.Del("Content-Range")
	res.Header.Del("ETag")
	res.Header.Del("Last-Modified")
	res.Header.Set("Content-Type", "text/plain; charset=utf-8")
	res.Header.Set("Cache-Control", "no-store")
}

// serveStaticPageResponse replaces a component response with a static page when the component named one, or when it returned 404 for a server-selected navigation. It reports whether the response now comes from the static directory.
func (a *app) serveStaticPageResponse(res *http.Response) (bool, error) {
	header := res.Header.Get(staticPageHeader)
	res.Header.Del(staticPageHeader)

	if res.Request == nil {
		return false, nil
	}

	pageRequest, _ := res.Request.Context().Value(staticPageRequestKey{}).(bool)
	if !pageRequest {
		return false, nil
	}

	var page string

	switch {
	case header != "" && res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices && res.StatusCode != http.StatusNoContent:
		page = header
	case header == "" && res.StatusCode == http.StatusNotFound:
		page = res.Request.URL.Path
	default:
		return false, nil
	}

	if !validStaticPagePath(page) {
		if header != "" {
			return false, fmt.Errorf("invalid static page %q", page)
		}

		return false, nil
	}

	path, ok := staticPageFile(a.staticDir(), page)
	if !ok {
		if header == "" {
			return false, nil
		}

		// A named page that does not exist is a 404 rather than a component error, so a catch-all handler can name the request path directly.
		notFoundStaticPage(res)

		return true, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open static page %q: %w", page, err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return false, fmt.Errorf("stat static page %q: %w", page, err)
	}

	if res.Body != nil {
		_ = res.Body.Close()
	}

	if header == "" {
		res.StatusCode = http.StatusOK
		res.Status = "200 OK"
	}

	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		contentType = "text/html; charset=utf-8"
	}

	res.Body = file
	res.ContentLength = info.Size()
	res.TransferEncoding = nil
	res.Trailer = nil
	res.Header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	res.Header.Set("Content-Type", contentType)
	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Range")
	res.Header.Del("Content-Disposition")
	res.Header.Del("ETag")
	res.Header.Del("Last-Modified")

	if res.Header.Get("Cache-Control") == "" {
		res.Header.Set("Cache-Control", "no-cache")
	}

	return true, nil
}

// serveStatic answers a request from the bundle's static/ directory. Real files win except for server-selected page navigations, trailing-slash directories resolve their index.html, and only an HTML navigation outside the app's server paths falls back to the SPA index.
func (a *app) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	dir, serverPaths, serverPages := a.staticRouting()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}

	// A server-selected page navigation reaches the component even when a real file exists, so the app can check the session and choose the page.
	if htmlNavigation(r) && serverPathMatch(r.URL.Path, serverPages) {
		return false
	}

	if path, original, encoding, hasVariants, ok := staticVariant(dir, r.URL.Path, r.Header.Get("Accept-Encoding")); ok {
		serveStaticFile(w, r, path, original, r.URL.Path, encoding, hasVariants)
		return true
	}

	if serverPathMatch(r.URL.Path, serverPaths) {
		return false
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
