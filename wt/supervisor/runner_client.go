package main

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type runnerStream struct {
	name    string
	events  chan runnerEvent
	started chan runnerEvent
}

type runnerClient struct {
	conn net.Conn

	writeMu   sync.Mutex
	mu        sync.Mutex
	streams   map[string]*runnerStream
	snapshot  []runnerAppInfo
	runnerPID int

	snapshotOnce sync.Once
	snapshotCh   chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	closeErr     error
}

func newRunnerClient(conn net.Conn) *runnerClient {
	c := &runnerClient{
		conn:       conn,
		streams:    map[string]*runnerStream{},
		snapshotCh: make(chan struct{}),
		done:       make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func dialRunner(path string, timeout time.Duration) (*runnerClient, []runnerAppInfo, int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			client := newRunnerClient(conn)
			select {
			case <-client.snapshotCh:
				return client, client.Snapshot(), client.RunnerPID(), nil
			case <-client.done:
				lastErr = client.Err()
				client.Close()
			case <-time.After(2 * time.Second):
				lastErr = errors.New("timed out waiting for the runner snapshot")
				client.Close()
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("runner did not accept a connection")
			}
			return nil, nil, 0, lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *runnerClient) readLoop() {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 64<<10), runnerScanBytes)
	for scanner.Scan() {
		var event runnerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			c.fail(fmt.Errorf("runner event: %w", err))
			return
		}
		c.dispatch(event)
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("runner connection closed")
	}
	c.fail(err)
}

func (c *runnerClient) dispatch(event runnerEvent) {
	switch event.Op {
	case "snapshot":
		c.mu.Lock()
		c.snapshot = append([]runnerAppInfo(nil), event.Snapshot...)
		c.runnerPID = event.RunnerPID
		for _, info := range event.Snapshot {
			if c.streams[info.Name] == nil {
				c.streams[info.Name] = &runnerStream{
					name:    info.Name,
					events:  make(chan runnerEvent, runnerOutputBuffer),
					started: make(chan runnerEvent, 1),
				}
			}
		}
		c.mu.Unlock()
		c.snapshotOnce.Do(func() { close(c.snapshotCh) })
	case "started", "error":
		c.mu.Lock()
		stream := c.streams[event.App]
		c.mu.Unlock()
		if stream == nil {
			return
		}
		select {
		case stream.started <- event:
		default:
		}
	case "exited":
		c.mu.Lock()
		stream := c.streams[event.App]
		c.mu.Unlock()
		if stream != nil {
			select {
			case stream.events <- event:
			default:
			}
		}
	default:
		c.mu.Lock()
		stream := c.streams[event.App]
		c.mu.Unlock()
		if stream == nil {
			return
		}
		select {
		case stream.events <- event:
		default:
			// A slow supervisor loses log lines rather than blocking every app's output.
		}
	}
}

func (c *runnerClient) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closeErr = err
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
		c.mu.Lock()
		streams := make([]*runnerStream, 0, len(c.streams))
		for _, stream := range c.streams {
			streams = append(streams, stream)
		}
		c.streams = map[string]*runnerStream{}
		c.mu.Unlock()
		for _, stream := range streams {
			select {
			case stream.events <- runnerEvent{Op: "exited", App: stream.name, Error: err.Error()}:
			default:
			}
		}
	})
}

func (c *runnerClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *runnerClient) Close() {
	c.fail(errors.New("runner client closed"))
}

func (c *runnerClient) send(request runnerRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return errors.New("runner connection closed")
	default:
	}
	_, err = c.conn.Write(data)
	return err
}

func (c *runnerClient) Snapshot() []runnerAppInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]runnerAppInfo(nil), c.snapshot...)
}

func (c *runnerClient) RunnerPID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runnerPID
}

func (c *runnerClient) newStream(name string) *runnerStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := c.streams[name]
	if stream == nil {
		stream = &runnerStream{
			name:    name,
			events:  make(chan runnerEvent, runnerOutputBuffer),
			started: make(chan runnerEvent, 1),
		}
		c.streams[name] = stream
	}
	return stream
}

// startStream replaces any previous stream for the app, so a restart cannot consume an old generation's buffered output or exit event.
func (c *runnerClient) startStream(name string) *runnerStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := &runnerStream{
		name:    name,
		events:  make(chan runnerEvent, runnerOutputBuffer),
		started: make(chan runnerEvent, 1),
	}
	c.streams[name] = stream
	return stream
}

func (c *runnerClient) Stream(name string) *runnerStream {
	return c.newStream(name)
}

func (c *runnerClient) Start(request runnerStartRequest) (*runnerStream, runnerEvent, error) {
	stream := c.startStream(request.Name)
	if err := c.send(runnerRequest{Op: "start", Start: &request}); err != nil {
		return nil, runnerEvent{}, err
	}
	select {
	case event := <-stream.started:
		if event.Op == "error" || event.Error != "" {
			return nil, event, errors.New(event.Error)
		}
		return stream, event, nil
	case <-time.After(runnerRequestTimeout):
		_ = c.send(runnerRequest{Op: "stop", App: request.Name})
		return nil, runnerEvent{}, errors.New("timed out waiting for the runner to start the app")
	case <-c.done:
		return nil, runnerEvent{}, c.Err()
	}
}

func (c *runnerClient) Go(name string) error {
	return c.send(runnerRequest{Op: "go", App: name})
}

func (c *runnerClient) Stop(name string) error {
	return c.send(runnerRequest{Op: "stop", App: name})
}

func (c *runnerClient) Shutdown() error {
	err := c.send(runnerRequest{Op: "shutdown"})
	c.Close()
	return err
}
