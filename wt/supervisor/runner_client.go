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

// dialRunner retries the runner socket until it accepts and sends its snapshot.
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

// readLoop decodes events until the connection fails.
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

// dispatch routes events to the matching app stream and records the initial snapshot.
func (c *runnerClient) dispatch(event runnerEvent) {
	switch event.Op {
	case runnerOpSnapshot:
		c.mu.Lock()
		c.snapshot = append([]runnerAppInfo(nil), event.Snapshot...)
		c.runnerPID = event.RunnerPID
		for _, info := range event.Snapshot {
			id := runnerKey(info.ID, info.Name)
			if c.streams[id] == nil {
				c.streams[id] = &runnerStream{
					name:    id,
					events:  make(chan runnerEvent, runnerOutputBuffer),
					started: make(chan runnerEvent, 1),
				}
			}
		}
		c.mu.Unlock()
		c.snapshotOnce.Do(func() { close(c.snapshotCh) })
	case runnerOpStarted, runnerOpError:
		c.mu.Lock()
		stream := c.streams[runnerKey(event.ID, event.App)]
		c.mu.Unlock()
		if stream == nil {
			return
		}
		select {
		case stream.started <- event:
		default:
		}
	case runnerOpExited:
		c.mu.Lock()
		stream := c.streams[runnerKey(event.ID, event.App)]
		c.mu.Unlock()
		if stream != nil {
			select {
			case stream.events <- event:
			default:
			}
		}
	default:
		c.mu.Lock()
		stream := c.streams[runnerKey(event.ID, event.App)]
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

// fail closes the client once and turns every open stream into an exit event.
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
			case stream.events <- runnerEvent{Op: runnerOpExited, App: stream.name, Error: err.Error()}:
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

// send writes one request to the runner connection.
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

// newStream returns the existing stream or creates one for a known app.
func (c *runnerClient) newStream(id string) *runnerStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := c.streams[id]
	if stream == nil {
		stream = &runnerStream{
			name:    id,
			events:  make(chan runnerEvent, runnerOutputBuffer),
			started: make(chan runnerEvent, 1),
		}
		c.streams[id] = stream
	}
	return stream
}

// startStream replaces any previous stream for the app, so a restart cannot consume an old generation's buffered output or exit event.
func (c *runnerClient) startStream(id string) *runnerStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := &runnerStream{
		name:    id,
		events:  make(chan runnerEvent, runnerOutputBuffer),
		started: make(chan runnerEvent, 1),
	}
	c.streams[id] = stream
	return stream
}

func (c *runnerClient) Stream(id string) *runnerStream {
	return c.newStream(id)
}

// Start sends a start request and waits for the runner's started event.
func (c *runnerClient) Start(request runnerStartRequest) (*runnerStream, runnerEvent, error) {
	stream := c.startStream(runnerKey(request.ID, request.Name))
	if err := c.send(runnerRequest{Op: runnerOpStart, Start: &request}); err != nil {
		return nil, runnerEvent{}, err
	}
	select {
	case event := <-stream.started:
		if event.Op == "error" || event.Error != "" {
			return nil, event, errors.New(event.Error)
		}
		return stream, event, nil
	case <-time.After(runnerRequestTimeout):
		_ = c.send(runnerRequest{Op: runnerOpStop, ID: stream.name})
		return nil, runnerEvent{}, errors.New("timed out waiting for the runner to start the app")
	case <-c.done:
		return nil, runnerEvent{}, c.Err()
	}
}

// Go releases a started app past its setup handshake.
func (c *runnerClient) Go(id string) error {
	return c.send(runnerRequest{Op: runnerOpGo, ID: id})
}

// Stop asks the runner to terminate one app.
func (c *runnerClient) Stop(id string) error {
	return c.send(runnerRequest{Op: runnerOpStop, ID: id})
}

// Shutdown asks the runner to exit and closes the client.
func (c *runnerClient) Shutdown() error {
	err := c.send(runnerRequest{Op: runnerOpShutdown})
	c.Close()
	return err
}
