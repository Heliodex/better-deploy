package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	var out struct {
		Text string `json:"text"`
	}
	ok := ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"text":"hi"}`)), 1<<10, "text", &out)
	if !ok || out.Text != "hi" {
		t.Fatalf("ok = %v, text = %q", ok, out.Text)
	}
}

func TestReadJSONRejectsMalformed(t *testing.T) {
	rec := httptest.NewRecorder()
	var out struct{}
	if ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{`)), 1<<10, "text", &out) {
		t.Fatal("malformed JSON was accepted")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
