package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticPageNamesTheSupervisorPage(t *testing.T) {
	rec := httptest.NewRecorder()
	StaticPage(rec, "/login/")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	if got := rec.Header().Get(StaticPageHeader); got != "/login/" {
		t.Errorf("%s = %q, want /login/", StaticPageHeader, got)
	}

	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

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

		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s cache-control = %q, want %q", tc.path, got, tc.cache)
		}
	}
}
