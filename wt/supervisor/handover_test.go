package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freeTestPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func waitHTTP(t *testing.T, url string, headers map[string]string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			if strings.EqualFold(key, "Host") {
				request.Host = value
				continue
			}
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			if response.StatusCode == want {
				response.Body.Close()
				return
			}
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not return %d: %v", url, want, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func supervisorAppPID(t *testing.T, base, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/apps/demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("app info status = %d", response.StatusCode)
	}
	var info appInfo
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info.PID
}

func waitLog(t *testing.T, path string, done func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if done(string(data)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the supervisor log:\n%s", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSupervisorHandoverKeepsAppPID(t *testing.T) {
	if os.Getenv("WT_HANDOVER_TEST") != "1" {
		t.Skip("set WT_HANDOVER_TEST=1 to run the exec handover test")
	}
	port := freeTestPort(t)
	appsDir := t.TempDir()
	stateDir := t.TempDir()
	appDir := filepath.Join(appsDir, "demo")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app.json"), []byte(`{"name":"demo","host":"demo.localhost","health":"/healthz"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(t.TempDir(), "supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	defer func() {
		data, _ := os.ReadFile(logFile.Name())
		t.Logf("supervisor log:\n%s", data)
	}()

	base := "http://127.0.0.1:" + port
	cmd := exec.Command(os.Args[0],
		"-listen", "127.0.0.1:"+port,
		"-apps", appsDir,
		"-state", stateDir,
		"-wasmtime", os.Args[0],
		"-token", "test",
		"-domain", "localhost",
	)
	cmd.Env = append(os.Environ(), "WT_TEST_SUPERVISOR=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	waitHTTP(t, base+"/healthz", nil, http.StatusOK)
	waitHTTP(t, base+"/", map[string]string{"Host": "demo.localhost"}, http.StatusNoContent)
	before := supervisorAppPID(t, base, "test")
	if before == 0 {
		t.Fatal("app has no pid")
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitLog(t, logFile.Name(), func(text string) bool {
		return strings.Contains(text, "handover: exec") && strings.Count(text, "supervisor listening") >= 2
	})
	waitHTTP(t, base+"/healthz", nil, http.StatusOK)
	waitHTTP(t, base+"/", map[string]string{"Host": "demo.localhost"}, http.StatusNoContent)
	after := supervisorAppPID(t, base, "test")
	if after != before {
		t.Errorf("app pid changed from %d to %d across handover", before, after)
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitLog(t, logFile.Name(), func(text string) bool {
		return strings.Count(text, "supervisor listening") >= 3
	})
	waitHTTP(t, base+"/healthz", nil, http.StatusOK)
	waitHTTP(t, base+"/", map[string]string{"Host": "demo.localhost"}, http.StatusNoContent)
	if second := supervisorAppPID(t, base, "test"); second != before {
		t.Errorf("app pid changed from %d to %d across the second handover", before, second)
	}

	data, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "handover: exec") {
		t.Errorf("log has no handover line:\n%s", text)
	}
	if strings.Count(text, "supervisor listening") < 3 {
		t.Errorf("log has fewer than three supervisor startups:\n%s", text)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("supervisor shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
}
