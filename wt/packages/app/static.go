package app

import (
	"io/fs"
	"net/http"
	"strings"
)

// StaticFiles serves an embedded frontend build, caching hashed /_app/ assets forever.
func StaticFiles(files fs.FS) http.Handler {
	server := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_app/") {
			w.Header().Set("cache-control", "public, max-age=31536000, immutable")
		}
		server.ServeHTTP(w, r)
	})
}
