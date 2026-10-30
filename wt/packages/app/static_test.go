package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticFilesCachesHashedAssets(t *testing.T) {
	handler := StaticFiles(fstest.MapFS{
		"_app/immutable/chunk.js": &fstest.MapFile{Data: []byte("js")},
		"index.html":              &fstest.MapFile{Data: []byte("html")},
	})

	cases := []struct {
		path, cache string
	}{
		{"/_app/immutable/chunk.js", "public, max-age=31536000, immutable"},
		{"/", ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.path, rec.Code)
		}
		if got := rec.Header().Get("cache-control"); got != tc.cache {
			t.Errorf("%s cache-control = %q, want %q", tc.path, got, tc.cache)
		}
	}
}
