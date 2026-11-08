package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Creates a secure, local-only network listener using a file on the disk
func ListenUnix(path string) (net.Listener, error) {
	// 0o700 permissions mean that only the owner of this account can read/write to this directory
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	existed := statErr == nil
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Only a directory we created is ours to chmod (to defeat the umask), never a pre-existing one like /tmp or $HOME
	if !existed {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("chmod socket dir: %w", err)
		}
	}

	// We try to connect to the socket as a client
	// If it succeeds it means another instance of the daemon is already running
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("daemon already running on %s", path)
	}

	// Only ever delete a leftover socket, never a regular file someone pointed us at
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat socket: %w", err)
	}

	// Bind with a restrictive umask so the socket is never briefly accessible to other users
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
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
