package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "spotd"

// xdg returns the env var only if it is an absolute path
func xdg(key string) string {
	if d := os.Getenv(key); filepath.IsAbs(d) {
		return d
	}

	return ""
}

// Figures out the correct place to permanently store the SQLite database
func DataDir() (string, error) {
	// If the user explicitly defined a Linux XDG data directory, respect it
	if d := xdg("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, appName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot find home directory: set XDG_DATA_HOME")
	}

	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", appName), nil
	}

	return filepath.Join(home, ".local", "share", appName), nil
}

func DBPath() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(d, "spotd.db"), nil
}

// SocketPath determines where to put the Unix Domain Socket
func SocketPath() (string, error) {
	if d := xdg("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, appName, "spotd.sock"), nil
	}

	// If the OS doesn't have a RAM disk set up, fall back to the database folder
	d, err := DataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(d, "spotd.sock"), nil
}
