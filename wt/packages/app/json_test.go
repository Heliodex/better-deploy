package app

import (
	"encoding/json/v2"
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

func TestJSONErrorField(t *testing.T) {
	cases := map[string]string{
		`field body pattern match: must match \S`:                                  "body",
		`field Name length: must be <= 40`:                                         "name",
		`field URL pattern match: must match ^https?`:                              "url",
		`json: cannot unmarshal number into Go struct field X.body of type string`: "fallback",
	}
	for message, want := range cases {
		if got := jsonErrorField(message, "fallback"); got != want {
			t.Errorf("jsonErrorField(%q) = %q, want %q", message, got, want)
		}
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

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.Code != "invalid" || body.Field != "text" || body.Error == "" {
		t.Errorf("body = %+v", body)
	}
}
