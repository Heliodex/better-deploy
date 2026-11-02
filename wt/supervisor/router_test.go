package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteTableMatch(t *testing.T) {
	rt := newRouteTable()
	rt.add(http.MethodGet, "/apps/{name}", func(http.ResponseWriter, *http.Request, map[string]string) {})
	rt.add(http.MethodPost, "/apps/{name}/jobs/{rest...}", func(http.ResponseWriter, *http.Request, map[string]string) {})

	cases := []struct {
		method, path string
		ok           bool
		params       map[string]string
		allow        string
	}{
		{http.MethodGet, "/apps/demo", true, map[string]string{"name": "demo"}, ""},
		{http.MethodGet, "/apps/demo/", true, map[string]string{"name": "demo"}, ""},
		{http.MethodPost, "/apps/demo/jobs", true, map[string]string{"name": "demo", "rest": ""}, ""},
		{http.MethodPost, "/apps/demo/jobs/abc/cancel", true, map[string]string{"name": "demo", "rest": "abc/cancel"}, ""},
		{http.MethodDelete, "/apps/demo", false, nil, "GET"},
		{http.MethodGet, "/apps/demo/jobs", false, nil, "POST"},
		{http.MethodGet, "/unknown", false, nil, ""},
	}
	for _, tc := range cases {
		route, params, allowed := rt.match(tc.method, tc.path)
		if (route != nil) != tc.ok {
			t.Errorf("%s %s matched = %t, want %t", tc.method, tc.path, route != nil, tc.ok)
			continue
		}

		if !tc.ok {
			if got := strings.Join(allowed, ", "); got != tc.allow {
				t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
			}

			continue
		}

		for key, want := range tc.params {
			if got := params[key]; got != want {
				t.Errorf("%s %s param %s = %q, want %q", tc.method, tc.path, key, got, want)
			}
		}
	}
}

func TestRouteTableServe(t *testing.T) {
	rt := newRouteTable()
	called := false

	rt.add(http.MethodGet, "/usage", func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
		called = true

		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/usage", nil))

	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("GET /usage = %d called=%t", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/usage", nil))

	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST /usage = %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /missing = %d, want 404", rec.Code)
	}
}
