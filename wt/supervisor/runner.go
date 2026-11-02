// The app runner owns every wasmtime process.
// It is a separate process so a supervisor exec can replace the control plane without touching the apps.
// A liveness pipe from the supervisor keeps the current crash behaviour: if the supervisor disappears, the runner stops the apps and exits.

package main

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	runnerArg            = "--wt-runner"
	runnerSocketEnv      = "WT_RUNNER_SOCKET"
	runnerLivenessFD     = 3
	runnerLivenessEnv    = "WT_RUNNER_LIVENESS_FD"
	runnerRequestTimeout = 30 * time.Second
	runnerOutputBuffer   = 1024
	runnerScanBytes      = 8 << 20
)

type runnerProcess struct {
	mu          sync.Mutex
	info        runnerAppInfo
	cmd         *exec.Cmd
	goWriter    *os.File
	pidReader   *os.File
	outputDone  chan struct{}
	portPending []byte
}

type runnerServer struct {
	socket   string
	listener net.Listener

	mu      sync.Mutex
	writeMu sync.Mutex
	conn    net.Conn
	apps    map[string]*runnerProcess
	closing bool
}

// runnerMain is the entry point for the --wt-runner process.
func runnerMain() {
	socket := os.Getenv(runnerSocketEnv)
	if socket == "" {
		fmt.Fprintln(os.Stderr, "wt runner: "+runnerSocketEnv+" is not set")
		os.Exit(2)
	}
	liveness := os.NewFile(runnerLivenessFD, "runner-liveness")
	if liveness == nil {
		fmt.Fprintln(os.Stderr, "wt runner: liveness pipe is missing")
		os.Exit(2)
	}
	syscall.CloseOnExec(runnerLivenessFD)
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wt runner: listen %s: %v\n", socket, err)
		os.Exit(1)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "wt runner: chmod %s: %v\n", socket, err)
		os.Exit(1)
	}
	r := &runnerServer{socket: socket, listener: listener, apps: map[string]*runnerProcess{}}
	go r.watchLiveness(liveness)
	r.acceptLoop()
}

func (r *runnerServer) watchLiveness(liveness *os.File) {
	var byte [1]byte
	for {
		if _, err := liveness.Read(byte[:]); err != nil {
			r.shutdown()
			os.Exit(0)
		}
	}
}

func (r *runnerServer) acceptLoop() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.handleConnection(conn)
	}
}

func (r *runnerServer) handleConnection(conn net.Conn) {
	r.mu.Lock()
	old := r.conn
	r.conn = conn
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	r.send(runnerEvent{Op: runnerOpSnapshot, Snapshot: r.snapshotApps(), RunnerPID: os.Getpid()})

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), runnerScanBytes)
	for scanner.Scan() {
		var request runnerRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			r.send(runnerEvent{Op: runnerOpError, Error: "invalid request: " + err.Error()})
			continue
		}
		r.handleRequest(conn, request)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("runner: read request: %v", err)
	}
	r.mu.Lock()
	if r.conn == conn {
		r.conn = nil
	}
	r.mu.Unlock()
	_ = conn.Close()
}

func (r *runnerServer) handleRequest(_ net.Conn, request runnerRequest) {
	id := runnerKey(request.ID, request.App)
	switch request.Op {
	case runnerOpStart:
		if request.Start == nil {
			r.send(runnerEvent{Op: runnerOpError, ID: id, App: request.App, Error: "missing start request"})
			return
		}
		r.start(*request.Start)
	case runnerOpGo:
		r.release(id)
	case runnerOpStop:
		r.stop(id)
	case runnerOpSignal:
		r.signal(id, syscall.Signal(request.Signal))
	case runnerOpSnapshot:
		r.send(runnerEvent{Op: runnerOpSnapshot, Snapshot: r.snapshotApps(), RunnerPID: os.Getpid()})
	case runnerOpShutdown:
		r.shutdown()
		os.Exit(0)
	default:
		r.send(runnerEvent{Op: runnerOpError, ID: id, App: request.App, Error: "unknown request " + request.Op})
	}
}

func (r *runnerServer) send(event runnerEvent) {
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("runner: encode event: %v", err)
		return
	}
	data = append(data, '\n')
	r.writeMu.Lock()
	_, err = conn.Write(data)
	r.writeMu.Unlock()
	if err != nil {
		r.mu.Lock()
		if r.conn == conn {
			r.conn = nil
		}
		r.mu.Unlock()
		_ = conn.Close()
	}
}

func (r *runnerServer) snapshotApps() []runnerAppInfo {
	r.mu.Lock()
	apps := make([]*runnerProcess, 0, len(r.apps))
	for _, app := range r.apps {
		apps = append(apps, app)
	}
	r.mu.Unlock()
	out := make([]runnerAppInfo, 0, len(apps))
	for _, app := range apps {
		app.mu.Lock()
		out = append(out, app.info)
		app.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newRunnerCommand builds the wasmtime or namespace-helper command and its pipes.
func newRunnerCommand(request runnerStartRequest, output *os.File) (*exec.Cmd, *os.File, *os.File, error) {
	if request.Namespaced {
		pidRead, pidWrite, err := os.Pipe()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("pid pipe: %w", err)
		}
		goRead, goWrite, err := os.Pipe()
		if err != nil {
			_ = pidRead.Close()
			_ = pidWrite.Close()
			return nil, nil, nil, fmt.Errorf("start pipe: %w", err)
		}
		cmd := exec.Command("/proc/self/exe", append([]string{nsInitArg}, request.Args...)...)
		cmd.Env = append(stripEnv(request.Env, nsPlanEnv), nsPlanEnv+"="+string(request.Plan))
		cmd.SysProcAttr = namespaceSysProcAttr(request.HostID, request.HostGID, request.AllowSetgroups)
		cmd.ExtraFiles = []*os.File{pidWrite, goRead}
		cmd.Stdout = output
		cmd.Stderr = output
		return cmd, goWrite, pidRead, nil
	}
	cmd := exec.Command(request.Path, request.Args...)
	cmd.Env = request.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd, nil, nil, nil
}

// registerProcess records the started app before its output and exit watchers begin.
func (r *runnerServer) registerProcess(request runnerStartRequest, cmd *exec.Cmd, goWriter, pidReader *os.File) *runnerProcess {
	app := &runnerProcess{
		info: runnerAppInfo{
			ID:           runnerKey(request.ID, request.Name),
			Name:         request.Name,
			Dir:          request.Dir,
			PID:          cmd.Process.Pid,
			Started:      time.Now().UTC(),
			UID:          request.UID,
			StorageToken: request.StorageToken,
			Namespaced:   request.Namespaced,
			Config:       request.Config,
		},
		cmd:        cmd,
		goWriter:   goWriter,
		pidReader:  pidReader,
		outputDone: make(chan struct{}),
	}
	if !request.Namespaced {
		app.info.WorkPID = cmd.Process.Pid
	}
	r.mu.Lock()
	r.apps[app.info.ID] = app
	r.mu.Unlock()
	return app
}

func (r *runnerServer) start(request runnerStartRequest) {
	id := runnerKey(request.ID, request.Name)
	sendError := func(message string) {
		r.send(runnerEvent{Op: runnerOpError, ID: id, App: request.Name, Error: message})
	}
	if request.Name == "" || request.Path == "" {
		sendError("name and path are required")
		return
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		sendError("output pipe: " + err.Error())
		return
	}

	cmd, goWriter, pidReader, err := newRunnerCommand(request, outputWrite)
	if err != nil {
		_ = outputRead.Close()
		_ = outputWrite.Close()
		sendError(err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		_ = outputRead.Close()
		_ = outputWrite.Close()
		if goWriter != nil {
			_ = goWriter.Close()
		}
		if pidReader != nil {
			_ = pidReader.Close()
		}
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
		sendError("start app: " + err.Error())
		return
	}
	_ = outputWrite.Close()
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
	cmd.ExtraFiles = nil

	app := r.registerProcess(request, cmd, goWriter, pidReader)
	r.send(runnerEvent{Op: runnerOpStarted, ID: app.info.ID, App: request.Name, PID: cmd.Process.Pid, WorkPID: app.info.WorkPID})
	go r.readOutput(app, outputRead)
	go r.waitApp(app)
	if pidReader != nil {
		go r.readWorkPID(app, pidReader)
	}
}

func (r *runnerServer) readOutput(app *runnerProcess, output *os.File) {
	defer output.Close()
	defer close(app.outputDone)
	buffer := make([]byte, 32<<10)
	for {
		n, err := output.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			r.send(runnerEvent{Op: runnerOpOutput, ID: app.info.ID, App: app.info.Name, Data: data})
			if port := app.observePort(data); port != 0 {
				r.send(runnerEvent{Op: runnerOpPort, ID: app.info.ID, App: app.info.Name, Port: port})
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *runnerServer) readWorkPID(app *runnerProcess, reader *os.File) {
	defer reader.Close()
	data, _ := io.ReadAll(io.LimitReader(reader, 64))
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	app.mu.Lock()
	app.info.WorkPID = pid
	app.mu.Unlock()
	r.send(runnerEvent{Op: runnerOpWork, ID: app.info.ID, App: app.info.Name, WorkPID: pid})
}

func (r *runnerServer) waitApp(app *runnerProcess) {
	err := app.cmd.Wait()
	<-app.outputDone
	code := 0
	message := ""
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = 128 + int(status.Signal())
			} else {
				code = exitErr.ExitCode()
			}
		}
		message = err.Error()
	}
	app.mu.Lock()
	id := app.info.ID
	name := app.info.Name
	goWriter := app.goWriter
	app.goWriter = nil
	pidReader := app.pidReader
	app.pidReader = nil
	app.mu.Unlock()
	if goWriter != nil {
		_ = goWriter.Close()
	}
	if pidReader != nil {
		_ = pidReader.Close()
	}
	r.mu.Lock()
	delete(r.apps, id)
	r.mu.Unlock()
	r.send(runnerEvent{Op: runnerOpExited, ID: id, App: name, Code: code, Error: message})
}

func (r *runnerServer) release(id string) {
	r.mu.Lock()
	app := r.apps[id]
	r.mu.Unlock()
	if app == nil {
		return
	}
	app.mu.Lock()
	writer := app.goWriter
	app.goWriter = nil
	app.mu.Unlock()
	if writer != nil {
		_, _ = writer.Write([]byte{1})
		_ = writer.Close()
	}
}

func (r *runnerServer) stop(id string) {
	r.signal(id, syscall.SIGTERM)
}

func (r *runnerServer) signal(id string, signal syscall.Signal) {
	r.mu.Lock()
	app := r.apps[id]
	r.mu.Unlock()
	if app == nil || signal == 0 {
		return
	}
	app.mu.Lock()
	pid := app.info.PID
	app.mu.Unlock()
	_ = syscall.Kill(pid, signal)
}

func (r *runnerServer) shutdown() {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.closing = true
	apps := make([]*runnerProcess, 0, len(r.apps))
	for _, app := range r.apps {
		apps = append(apps, app)
	}
	r.mu.Unlock()
	if r.listener != nil {
		_ = r.listener.Close()
	}
	for _, app := range apps {
		app.mu.Lock()
		pid := app.info.PID
		app.mu.Unlock()
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		count := len(r.apps)
		r.mu.Unlock()
		if count == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, app := range apps {
		app.mu.Lock()
		pid := app.info.PID
		app.mu.Unlock()
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(r.socket)
}

// observePort scans a chunk for wasmtime's startup address and records the first port.
func (p *runnerProcess) observePort(data []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.info.Port != 0 {
		return 0
	}
	p.portPending = append(p.portPending, data...)
	for {
		index := bytes.IndexByte(p.portPending, '\n')
		if index < 0 {
			break
		}
		line := string(p.portPending[:index])
		p.portPending = p.portPending[index+1:]
		if port, ok := parseServePort(line); ok {
			p.info.Port = port
			return port
		}
	}
	if len(p.portPending) > 8<<10 {
		p.portPending = append([]byte(nil), p.portPending[len(p.portPending)-(8<<10):]...)
	}
	return 0
}

// runnerSocketPath is where the supervisor connects to the runner.
func runnerSocketPath(stateDir string) string {
	return filepath.Join(stateDir, ".runner.sock")
}
