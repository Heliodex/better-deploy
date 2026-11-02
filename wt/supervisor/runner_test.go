package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func startTestRunner(t *testing.T) (*runnerClient, *os.File, string) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "runner.sock")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], runnerArg)

	cmd.Env = append(minimalEnv(), runnerSocketEnv+"="+socket, runnerLivenessEnv+"=3")
	cmd.ExtraFiles = []*os.File{read}

	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	_ = read.Close()

	client, _, _, err := dialRunner(socket, 5*time.Second)
	if err != nil {
		_ = write.Close()
		_ = cmd.Process.Kill()

		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = write.Close()
		client.Close()

		_ = cmd.Wait()
	})

	return client, write, socket
}

func waitRunnerEvent(t *testing.T, stream *runnerStream, op string) runnerEvent {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		select {
		case event := <-stream.events:
			if event.Op == op {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", op)
		}
	}
}

func TestRunnerStartsStreamsAndStopsAnApp(t *testing.T) {
	client, _, _ := startTestRunner(t)

	stream, started, err := client.Start(runnerStartRequest{
		ID:   "demo",
		Name: "demo",
		Dir:  t.TempDir(),
		Path: os.Args[0],
		Args: []string{"--addr", "127.0.0.1:0"},
		Env:  minimalEnv(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if started.PID <= 0 {
		t.Fatalf("started pid = %d", started.PID)
	}

	port := waitRunnerEvent(t, stream, "port").Port
	if port <= 0 {
		t.Fatalf("port = %d", port)
	}

	if err := client.Stop("demo"); err != nil {
		t.Fatal(err)
	}

	waitRunnerEvent(t, stream, "exited")

	if len(client.Snapshot()) != 0 {
		t.Error("stopped app is still in the initial runner snapshot")
	}
}

func TestRunnerSnapshotSurvivesAReconnect(t *testing.T) {
	client, _, socket := startTestRunner(t)

	stream, _, err := client.Start(runnerStartRequest{
		ID:   "demo",
		Name: "demo",
		Dir:  t.TempDir(),
		Path: os.Args[0],
		Args: []string{"--addr", "127.0.0.1:0"},
		Env:  minimalEnv(),
	})
	if err != nil {
		t.Fatal(err)
	}

	port := waitRunnerEvent(t, stream, "port").Port

	client.Close()

	restarted, snapshot, _, err := dialRunner(socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	if len(snapshot) != 1 || snapshot[0].Name != "demo" || snapshot[0].Port != port {
		t.Fatalf("snapshot after reconnect = %+v", snapshot)
	}

	if err := restarted.Stop("demo"); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerLivenessStopsApps(t *testing.T) {
	client, write, socket := startTestRunner(t)

	_, _, err := client.Start(runnerStartRequest{
		ID:   "demo",
		Name: "demo",
		Dir:  t.TempDir(),
		Path: os.Args[0],
		Args: []string{"--addr", "127.0.0.1:0"},
		Env:  minimalEnv(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = write.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("runner did not stop after its liveness pipe closed")
}

func TestRunnerLaunchServes(t *testing.T) {
	s := newLifecycleSupervisor(t)
	client, _, _ := startTestRunner(t)
	s.runner = client

	a := spawnTestApp(t, s, "demo")
	if a.port == 0 || !a.ready {
		t.Fatalf("port = %d, ready = %t", a.port, a.ready)
	}

	if a.cmd == nil || a.cmd.Process == nil || a.cmd.Process.Pid == 0 {
		t.Fatal("runner launch has no child pid")
	}

	if a.workPID != a.cmd.Process.Pid {
		t.Errorf("workPID = %d, want %d for a trusted app", a.workPID, a.cmd.Process.Pid)
	}
}

func TestAdoptAppFromRunner(t *testing.T) {
	client, _, socket := startTestRunner(t)
	appsDir := t.TempDir()

	dir := filepath.Join(appsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	config := `{"name":"demo","host":"demo.localhost","health":"/"}`
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.cwasm"), []byte("cwasm"), 0o644); err != nil {
		t.Fatal(err)
	}

	stream, started, err := client.Start(runnerStartRequest{
		ID:     "demo",
		Name:   "demo",
		Dir:    dir,
		Path:   os.Args[0],
		Args:   []string{"--addr", "127.0.0.1:0"},
		Env:    minimalEnv(),
		Config: []byte(config),
	})
	if err != nil {
		t.Fatal(err)
	}

	port := waitRunnerEvent(t, stream, "port").Port

	adopted, snapshot, _, err := dialRunner(socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer adopted.Close()

	if len(snapshot) != 1 || snapshot[0].PID != started.PID || snapshot[0].Port != port {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	s := testSupervisor()
	s.cfg.appsDir = appsDir
	s.cfg.stateDir = t.TempDir()
	s.cfg.domain = "localhost"
	s.cfg.wasmtime = os.Args[0]
	s.cfg.wasmtimeTarget = "x86_64-test"
	s.runner = adopted

	s.runnerSnapshot = snapshot
	if err := s.loadApps(); err != nil {
		t.Fatal(err)
	}

	a := s.lookup("demo")
	if a == nil {
		t.Fatal("adopted app is not registered")
	}

	if a.port != port || !a.ready {
		t.Errorf("adopted port = %d, ready = %t", a.port, a.ready)
	}

	if a.cmd == nil || a.cmd.Process == nil || a.cmd.Process.Pid != started.PID {
		t.Errorf("adopted cmd = %+v", a.cmd)
	}

	if a.proxy == nil {
		t.Error("adopted app has no proxy")
	}

	s.stopping.Store(true)
	s.drainAndStop(a, "test")

	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopted app did not stop")
	}
}

func TestInheritedListenerKeepsAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	file, err := listenerFile(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if err := clearCloseOnExec(file); err != nil {
		t.Fatal(err)
	}

	t.Setenv(storageFDEnv, strconv.Itoa(int(file.Fd())))

	inherited, ok, err := inheritedListener(storageFDEnv)
	if err != nil || !ok {
		t.Fatalf("inherited listener = %v, %t, %v", inherited, ok, err)
	}
	defer inherited.Close()

	if inherited.Addr().String() != listener.Addr().String() {
		t.Errorf("address = %s, want %s", inherited.Addr(), listener.Addr())
	}
}
