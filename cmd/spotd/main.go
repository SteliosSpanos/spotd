package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/SteliosSpanos/spotd/internal/auth"
	"github.com/SteliosSpanos/spotd/internal/config"
)

// These variables are injected at compile time using Go's -ldflags
// When you build the app, the compiler fills these in with the Git commit hash
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var errNoClientID = errors.New("missing client ID: pass -client-id or set SPOTD_CLIENT_ID")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "spotd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("missing subcommand")
	}

	// This intercepts OS signals like SIGINT and SIGTERM
	// Instead of violently crashing the app, it gracefully cancels this context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// If the context is cancelled, ensure the stop function is called to free up OS resources
	context.AfterFunc(ctx, stop)

	switch args[0] {
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		// fs.String() returns *string
		clientID := fs.String("client-id", os.Getenv("SPOTD_CLIENT_ID"), "Spotify app client ID")

		_ = fs.Parse(args[1:])
		return runLogin(ctx, *clientID)
	case "logout":
		if err := (auth.KeyringStore{}).Clear(); err != nil {
			return fmt.Errorf("clear keyring: %w", err)
		}

		fmt.Println("Logged out")
		return nil
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		clientID := fs.String("client-id", os.Getenv("SPOTD_CLIENT_ID"), "Spotify app client ID")
		sock := fs.String("socket", "", "unix path (default $XDG_RUNTIME_DIR/spotd/spotd.sock)")
		dbPath := fs.String("db", "", "SQLite database path (default $XDG_DATA_HOME/spotd/spotd.db)")
		logLevel := fs.String("log-level", "info", "debug, info, warn or error")
		logFormat := fs.String("log-format", "text", "text or json")
		_ = fs.Parse(args[1:])

		if *clientID == "" {
			return errNoClientID
		}

		var err error
		if *sock == "" {
			if *sock, err = config.SocketPath(); err != nil {
				return err
			}
		}

		if *dbPath == "" {
			if *dbPath, err = config.DBPath(); err != nil {
				return err
			}
		}

		log, err := newLogger(os.Stderr, *logLevel, *logFormat)
		if err != nil {
			return err
		}

		return runDaemon(ctx, log, *clientID, *sock, *dbPath)
	case "version":
		fmt.Printf("spotd %s (commit %s, built %s)\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: spotd <command> [flags]

commands:
  login     authorize spotd with your Spotify account
  logout    remove the stored refresh token
  daemon    run the background poller and gRPC server
  version   print version info

run "spotd <command> -h" for the flags of a command
`)
}

func runLogin(ctx context.Context, clientID string) error {
	if clientID == "" {
		return errNoClientID
	}

	tok, err := auth.Login(ctx, auth.Config(clientID), auth.OpenBrowser)
	if err != nil {
		return err
	}

	// We need a Refresh Token to keep the daemon running in the background forever without asking the user to log in again
	if tok.RefreshToken == "" {
		return errors.New("spotify returned no refresh token")
	}

	if err := (auth.KeyringStore{}).SaveRefreshToken(tok.RefreshToken); err != nil {
		return err
	}

	fmt.Println("Logged in. Start the daemon with `spotd daemon`")
	return nil
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lv}

	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid -log-format %q", format)
	}
}
