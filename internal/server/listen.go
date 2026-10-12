package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Creates a secure, local-only network listener using a file on the disk
func ListenUnix(path string) (net.Listener, error) {
	// 0o700 permissions mean that only the owner of this account can read/write to this directory
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// We try to connect to the socket as a client
	// If it succeeds it means another instance of the daemon is already running
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("daemon already running on %s", path)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// 0o600 restricts the socket file so only the user who ran the app can read/write to it
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return ln, nil
}
