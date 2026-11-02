package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// parseAdminListen accepts a TCP address ("127.0.0.1:8081") or "unix:/path/to.sock".
func parseAdminListen(value string) (network, address string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", errors.New("empty address")
	}

	if path, ok := strings.CutPrefix(value, "unix:"); ok {
		if strings.TrimSpace(path) == "" {
			return "", "", errors.New("unix admin listener needs a path")
		}

		return "unix", path, nil
	}

	if address, ok := strings.CutPrefix(value, "tcp:"); ok {
		if strings.TrimSpace(address) == "" {
			return "", "", errors.New("tcp admin listener needs an address")
		}

		return "tcp", address, nil
	}

	return "tcp", value, nil
}

// listenAdmin binds the separate admin listener. A Unix socket is created 0600 after removing a stale socket, and cleanup removes it.
func listenAdmin(network, address string) (net.Listener, func(), error) {
	if network != "unix" {
		var listenConfig net.ListenConfig

		listener, err := listenConfig.Listen(context.Background(), network, address)
		if err != nil {
			return nil, nil, err
		}

		return listener, func() { _ = listener.Close() }, nil
	}

	if err := os.MkdirAll(filepath.Dir(address), 0o750); err != nil {
		return nil, nil, fmt.Errorf("admin socket dir: %w", err)
	}

	if info, err := os.Lstat(address); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("admin socket path %s exists and is not a socket", address)
		}

		if err := os.Remove(address); err != nil {
			return nil, nil, fmt.Errorf("remove stale admin socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("stat admin socket: %w", err)
	}

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(context.Background(), "unix", address)
	if err != nil {
		return nil, nil, err
	}

	if err := os.Chmod(address, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(address)

		return nil, nil, fmt.Errorf("chmod admin socket: %w", err)
	}

	cleanup := func() {
		_ = listener.Close()
		_ = os.Remove(address)
	}

	return listener, cleanup, nil
}
