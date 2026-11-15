// The runner protocol: newline-delimited JSON requests and events over a Unix socket.

package main

import "time"

type (
	runnerAppInfo struct {
		ID string `json:"id,omitempty"`
		Name string `json:"name"`
		Dir string `json:"dir"`
		PID int `json:"pid"`
		WorkPID int `json:"workPid,omitempty"`
		Port int `json:"port,omitempty"`
		Started time.Time `json:"started"`
		UID int `json:"uid,omitempty"`
		StorageToken string `json:"storageToken,omitempty"`
		Namespaced bool `json:"namespaced,omitempty"`
		Config []byte `json:"config,omitempty"`
	}

	runnerEvent struct {
		Op string `json:"op"`
		ID string `json:"id,omitempty"`
		App string `json:"app,omitempty"`
		PID int `json:"pid,omitempty"`
		WorkPID int `json:"workPid,omitempty"`
		Port int `json:"port,omitempty"`
		Data []byte `json:"data,omitempty"`
		Code int `json:"code,omitempty"`
		Error string `json:"error,omitempty"`
		Snapshot []runnerAppInfo `json:"snapshot,omitempty"`
		RunnerPID int `json:"runnerPid,omitempty"`
	}

	runnerStartRequest struct {
		ID string `json:"id,omitempty"`
		Name string `json:"name"`
		Dir string `json:"dir"`
		Path string `json:"path"`
		Args []string `json:"args,omitempty"`
		Env []string `json:"env,omitempty"`
		Config []byte `json:"config,omitempty"`
		Namespaced bool `json:"namespaced,omitempty"`
		Plan []byte `json:"plan,omitempty"`
		HostID int `json:"hostID,omitempty"`
		HostGID int `json:"hostGID,omitempty"`
		AllowSetgroups bool `json:"allowSetgroups,omitempty"`
		UID int `json:"uid,omitempty"`
		StorageToken string `json:"storageToken,omitempty"`
	}

	// runnerConn is the supervisor side of the app runner.
	runnerConn interface {
		Start(request runnerStartRequest) (*runnerStream, runnerEvent, error)
		Go(name string) error
		Stop(name string) error
		Stream(name string) *runnerStream
		Close()
	}

	runnerRequest struct {
		Op string `json:"op"`
		ID string `json:"id,omitempty"`
		App string `json:"app,omitempty"`
		Start *runnerStartRequest `json:"start,omitempty"`
		Signal int `json:"signal,omitempty"`
	}
)

const (
	runnerOpError = "error"
	runnerOpExited = "exited"
	runnerOpGo = "go"
	runnerOpOutput = "output"
	runnerOpPort = "port"
	runnerOpShutdown = "shutdown"
	runnerOpSignal = "signal"
	runnerOpSnapshot = "snapshot"

	// Request operations sent from the supervisor to the runner.
	runnerOpStart = "start"

	// Event operations sent from the runner to the supervisor.
	runnerOpStarted = "started"
	runnerOpStop = "stop"
	runnerOpWork = "work"
)
