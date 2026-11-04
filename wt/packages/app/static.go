package app

import (
	"io/fs"
	"net/http"
	"strings"
)

// StaticPageHeader asks the supervisor to answer with a page from the app's static/ directory instead of the component's body. An app declares the navigation prefixes it controls in app.json with server_pages.
const StaticPageHeader = "X-Wt-Static-Page"

// StaticPage writes an empty successful response whose StaticPageHeader names the page the supervisor should serve. The path resolves like a normal static request, so "/login/" finds a directory index and "/login" finds login.html. Redirects, status codes and other responses should use the usual http helpers instead.
func StaticPage(w http.ResponseWriter, page string) {
	w.Header().Set(StaticPageHeader, page)
	w.WriteHeader(http.StatusOK)
}

// StaticFiles serves an embedded frontend build, caching hashed /_app/ assets forever.
func StaticFiles(files fs.FS) http.Handler {
	server := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_app/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		server.ServeHTTP(w, r)
	})
}
