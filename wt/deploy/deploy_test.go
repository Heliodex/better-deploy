package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckDeploySecretsPassesWithoutRefs(t *testing.T) {
	if err := checkDeploySecrets(&deployConfig{}, manifest{Name: "demo", Env: map[string]string{"PLAIN": "value"}}); err != nil {
		t.Errorf("no secret refs should skip the check: %v", err)
	}
}

func TestCheckDeploySecretsFindsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/demo/secrets" {
			t.Errorf("path = %q, want the app secrets list", r.URL.Path)
		}

		_, _ = w.Write([]byte(`{"keys":["HAVE"]}`))
	}))
	defer server.Close()

	cfg := &deployConfig{host: server.URL}
	m := manifest{Name: "demo", Env: map[string]string{
		"FIRST":  "secret://HAVE",
		"SECOND": "secret://MISSING",
	}}

	err := checkDeploySecrets(cfg, m)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing secret error = %v, want MISSING", err)
	}

	if strings.Contains(err.Error(), "HAVE") {
		t.Errorf("present secret reported missing: %v", err)
	}
}

func TestCheckDeploySecretsAcceptsAllPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":["HAVE"]}`))
	}))
	defer server.Close()

	cfg := &deployConfig{host: server.URL}
	m := manifest{Name: "demo", Env: map[string]string{"FIRST": "secret://HAVE"}}

	if err := checkDeploySecrets(cfg, m); err != nil {
		t.Errorf("present secrets should pass: %v", err)
	}
}
