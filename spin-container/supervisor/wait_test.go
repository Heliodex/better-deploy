package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sleepingApp returns an app whose process is alive but whose port is given, so awaitApp's liveness check passes.
func sleepingApp(t *testing.T, port int) *App {
	t.Helper()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &App{Name: "test", Port: port, Health: "/", cmd: cmd}
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAwaitApp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	s := &Supervisor{}
	app := sleepingApp(t, serverPort(t, server))
	if err := s.awaitApp(app, 2*time.Second); err != nil {
		t.Fatalf("awaitApp: %v", err)
	}
}

func TestAwaitAppTimeout(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	s := &Supervisor{}
	app := sleepingApp(t, port)
	err = s.awaitApp(app, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "no response") {
		t.Errorf("error = %q, want a no response error", err)
	}
}

func TestAwaitAppStoppedProcess(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	s := &Supervisor{}
	app := &App{Name: "test", Port: port, Health: "/"}
	if err := s.awaitApp(app, time.Second); err == nil || !strings.Contains(err.Error(), "process exited") {
		t.Fatalf("error = %v, want a process exited error", err)
	}
}
