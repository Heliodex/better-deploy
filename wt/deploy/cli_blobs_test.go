package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunBlobsList(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"blobs":[{"digest":"` + strings.Repeat("a", 64) + `","size":3,"contentType":"text/plain"}]}`))
	}))
	defer server.Close()

	if err := runBlobs([]string{"blobbox", "list", "--host", server.URL, "--token", "secret"}); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/apps/blobbox/blobs" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestRunBlobsGC(t *testing.T) {
	var (
		gotPath, gotAuth string
		got              map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		gotAuth = r.Header.Get("Authorization")
		if err := json.UnmarshalRead(r.Body, &got); err != nil {
			t.Errorf("body: %v", err)
		}

		_, _ = w.Write([]byte(`{"deleted":["` + strings.Repeat("a", 64) + `"],"dryRun":true,"olderThan":"24h"}`))
	}))
	defer server.Close()

	if err := runBlobs([]string{
		"blobbox", "gc",
		"--older-than", "24h",
		"--dry-run",
		"--keep", strings.Repeat("b", 64),
		"--host", server.URL,
		"--token", "secret",
	}); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/apps/blobbox/blobs/gc" || gotAuth != "Bearer secret" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}

	if got["olderThan"] != "24h" || got["dryRun"] != true {
		t.Errorf("payload = %+v", got)
	}

	keep, ok := got["keep"].([]any)
	if !ok || len(keep) != 1 || keep[0] != strings.Repeat("b", 64) {
		t.Errorf("keep = %+v", got["keep"])
	}
}

func TestRunBlobsDelete(t *testing.T) {
	var gotMethod, gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	digest := strings.Repeat("a", 64)

	if err := runBlobs([]string{"blobbox", "delete", digest, "--host", server.URL}); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodDelete || gotPath != "/apps/blobbox/blobs/"+digest {
		t.Errorf("method=%q path=%q", gotMethod, gotPath)
	}
}
