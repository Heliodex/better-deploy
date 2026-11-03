//go:build !wasip1

package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heliodex/better-deploy/wt/packages/storage"
)

func TestBlobAppListsFiles(t *testing.T) {
	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client := server.NewClient(server.Client())
	application := newBlobApp(client, server.Client())
	if err := application.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	application.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list files = %d: %s", rec.Code, rec.Body.String())
	}
	var body filesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 0 {
		t.Errorf("files = %+v, want none", body.Files)
	}
}
