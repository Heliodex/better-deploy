package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func appFetchTarget(t *testing.T, s *supervisor, name string, allow []string) (*app, *storageServer, string) {
	t.Helper()
	storage := &storageServer{tokens: map[string]string{}}
	s.storage = storage
	token := storage.tokenFor(name)
	s.apps["caller"] = &app{name: "caller"}
	a := &app{
		name:  name,
		cfg:   appConfig{Host: name + ".localhost", AllowCallers: allow},
		cmd:   &exec.Cmd{},
		ready: true,
	}
	a.proxy = s.newProxy(a)
	s.apps[name] = a
	return a, storage, token
}

func TestAppFetchSignsCallerAndForwards(t *testing.T) {
	s := testSupervisor()
	target, storage, targetToken := appFetchTarget(t, s, "target", []string{"caller"})
	callerToken := storage.tokenFor("caller")

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/items" || r.URL.RawQuery != "limit=1" {
			t.Errorf("target URL = %q", r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("caller credentials were forwarded to the target")
		}
		if r.Header.Get(appCallerHeader) != "caller" {
			t.Errorf("caller header = %q", r.Header.Get(appCallerHeader))
		}
		if r.Header.Get(appCallerHeader) == "spoofed" {
			t.Error("caller-supplied identity survived")
		}
		timestamp := r.Header.Get(appTimestampHeader)
		body, _ := io.ReadAll(r.Body)
		bodyHash := appBodySHA256(body)
		if got := r.Header.Get(appBodyHashHeader); got != bodyHash {
			t.Errorf("body hash = %q, want %q", got, bodyHash)
		}
		if want := appCallSignatureV2(targetToken, "caller", r.Method, r.URL.RequestURI(), timestamp, bodyHash); r.Header.Get(appSignatureV2Header) != want {
			t.Errorf("v2 signature = %q, want %q", r.Header.Get(appSignatureV2Header), want)
		}
		if r.Header.Get(requestIDHeader) != "request-1" {
			t.Errorf("request id = %q", r.Header.Get(requestIDHeader))
		}
		if full := r.Header.Get(spinFullURLHeader); full != "http://target.localhost/v1/items?limit=1" {
			t.Errorf("spin-full-url = %q", full)
		}
		if r.Header.Get(spinClientAddrHeader) == "" {
			t.Error("spin-client-addr is empty")
		}
		if string(body) != `{"value":"one"}` {
			t.Errorf("body = %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer targetServer.Close()
	address, ok := targetServer.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", targetServer.Listener.Addr())
	}
	target.port = address.Port
	target.proxy = s.newProxy(target)

	req := httptest.NewRequest(http.MethodPost, "/apps/target/v1/items?limit=1", strings.NewReader(`{"value":"one"}`))
	req.Header.Set("Authorization", "Bearer "+callerToken)
	req.Header.Set(appCallerHeader, "spoofed")
	req.Header.Set(requestIDHeader, "request-1")
	rec := httptest.NewRecorder()
	s.handleAppFetch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("response = %q", rec.Body.String())
	}
}

func TestAppFetchRejectsUnauthorizedAndDisallowedCallers(t *testing.T) {
	s := testSupervisor()
	_, storage, _ := appFetchTarget(t, s, "target", nil)
	callerToken := storage.tokenFor("caller")

	req := httptest.NewRequest(http.MethodGet, "/apps/target/", nil)
	rec := httptest.NewRecorder()
	s.handleAppFetch(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/target/", nil)
	req.Header.Set("Authorization", "Bearer "+callerToken)
	rec = httptest.NewRecorder()
	s.handleAppFetch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disallowed caller = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/missing/", nil)
	req.Header.Set("Authorization", "Bearer "+callerToken)
	rec = httptest.NewRecorder()
	s.handleAppFetch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing target = %d, want 404", rec.Code)
	}
}

func TestAppFetchPathParsing(t *testing.T) {
	for _, test := range []struct {
		path, target, rest string
		ok                 bool
	}{
		{"/apps/notes", "notes", "/", true},
		{"/apps/notes/", "notes", "/", true},
		{"/apps/notes/v1/items?x=1", "notes", "/v1/items?x=1", true},
		{"/apps/../etc/passwd", "", "", false},
		{"/apps/Notes/", "", "", false},
		{"/other/notes", "", "", false},
	} {
		target, rest, ok := parseAppFetchPath(test.path)
		if ok != test.ok || target != test.target || rest != test.rest {
			t.Errorf("parse %q = %q, %q, %v; want %q, %q, %v", test.path, target, rest, ok, test.target, test.rest, test.ok)
		}
	}
}

func TestAppArgsExposeInternalFetchAndPerAppURLs(t *testing.T) {
	s := testSupervisor()
	s.fetch = &appFetchServer{baseURL: "http://127.0.0.1:9000"}
	s.storage = &storageServer{tokens: map[string]string{}}
	s.apps["notes-api"] = &app{name: "notes-api"}
	s.apps["later"] = &app{name: "later"}
	a := &app{name: "frontend", cfg: appConfig{}}

	args, env, err := s.appArgs(a, "main.cwasm")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("env entry %q has no =", entry)
		}
		values[key] = value
	}
	if values[appNameEnv] != "frontend" || values[appFetchURLEnv] != "http://127.0.0.1:9000" {
		t.Errorf("fetch env = %v", values)
	}
	if values[appURLEnvName("notes-api")] != "http://127.0.0.1:9000/apps/notes-api" {
		t.Errorf("notes URL = %q", values[appURLEnvName("notes-api")])
	}
	if values[appURLEnvName("later")] != "http://127.0.0.1:9000/apps/later" {
		t.Errorf("later URL = %q", values[appURLEnvName("later")])
	}
	if values[appTokenEnv] == "" || values[appTokenEnv] != values["WT_STORAGE_TOKEN"] {
		t.Errorf("app token = %q, storage token = %q", values[appTokenEnv], values["WT_STORAGE_TOKEN"])
	}
	argv := strings.Join(args, " ")
	for _, value := range []string{values[appFetchURLEnv], values[appTokenEnv], values[appURLEnvName("notes-api")]} {
		if value != "" && strings.Contains(argv, value) {
			t.Errorf("argv leaked %q: %q", value, argv)
		}
	}
}

func TestValidateAppFetchListen(t *testing.T) {
	for _, value := range []string{"127.0.0.1:0", "127.0.0.1:9010", "localhost:9010", "[::1]:9010"} {
		if err := validateAppFetchListen(value); err != nil {
			t.Errorf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "0.0.0.0:9010", "192.0.2.1:9010", "example.com:9010", "127.0.0.1"} {
		if err := validateAppFetchListen(value); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}
