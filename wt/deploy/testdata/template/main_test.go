package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"__CONTRACT_IMPORT__"
	"github.com/heliodex/better-deploy/wt/packages/storage"
)

func TestNotesHandler(t *testing.T) {
	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}
	application := newNotesApp(server.NewClient(server.Client()))
	if err := application.migrate(t.Context()); err != nil {
		server.Close()
		t.Fatal(err)
	}
	handler := application.handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{"text":"hello"}`)))
	if rec.Code != http.StatusCreated {
		server.Close()
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notes", nil))
	if rec.Code != http.StatusOK {
		server.Close()
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var listed contract.NotesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		server.Close()
		t.Fatal(err)
	}
	if len(listed.Notes) != 1 || listed.Notes[0].Text != "hello" {
		t.Errorf("notes = %+v", listed.Notes)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{"text":""}`)))
	if rec.Code != http.StatusBadRequest {
		server.Close()
		t.Fatalf("invalid create = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		server.Close()
		t.Fatal(err)
	}
	if body["code"] != "invalid" || body["field"] != "text" {
		t.Errorf("validation body = %+v", body)
	}

	server.Close()
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notes", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("storage failure = %d, want 500", rec.Code)
	}
}
