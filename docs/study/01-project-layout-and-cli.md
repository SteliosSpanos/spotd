# 01 — Project layout & CLI subcommands

## Concepts

- **One module, one binary, several subcommands** (`spotd login | daemon | tui`). Each subcommand runs as its own OS process; they share code, not memory.
- **`cmd/` vs `internal/`**: `cmd/spotd/main.go` is thin wiring. Real logic lives in `internal/...` so it is unimportable by other modules and easy to test.
- **Dependency direction**: high-level packages (daemon) depend on low-level ones (spotify, store), never the other way round. Interfaces are declared by the *consumer*.

## Recommended layout

```
spotd/
├── cmd/spotd/main.go          # parse args, dispatch subcommand, exit code
├── internal/
│   ├── auth/                  # PKCE login flow, keyring-backed TokenSource
│   ├── spotify/               # thin HTTP client + DTOs
│   ├── poller/                # fast + slow loops
│   ├── broadcast/             # pub/sub
│   ├── store/                 # sqlite open, migrations, sqlc-generated code
│   │   ├── migrations/*.sql
│   │   ├── queries/*.sql
│   │   └── db/                # sqlc output (generated)
│   ├── server/                # gRPC service implementations
│   ├── jobs/                  # background playlist jobs
│   ├── tui/                   # Bubble Tea models
│   └── config/                # paths, flags, env
├── proto/spotd/v1/spotd.proto
├── gen/spotd/v1/              # buf output (generated)
├── buf.yaml  buf.gen.yaml  sqlc.yaml
├── .golangci.yml  .goreleaser.yaml
└── .github/workflows/ci.yml
```

Generated code: either commit it (simple `go install`, reviewers can read it) **and** check in CI that it is up to date (`buf generate && sqlc generate && git diff --exit-code`). That CI check is a good signal of discipline.

## Subcommand dispatch with the standard library

You do not need Cobra for three commands. `flag.NewFlagSet` per subcommand is enough and shows you understand the stdlib.

```go
// cmd/spotd/main.go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "spotd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing subcommand")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		clientID := fs.String("client-id", os.Getenv("SPOTD_CLIENT_ID"), "Spotify app client ID")
		_ = fs.Parse(args[1:])
		return runLogin(ctx, *clientID)
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		sock := fs.String("socket", defaultSocketPath(), "unix socket path")
		_ = fs.Parse(args[1:])
		return runDaemon(ctx, *sock)
	case "tui":
		fs := flag.NewFlagSet("tui", flag.ExitOnError)
		sock := fs.String("socket", defaultSocketPath(), "unix socket path")
		_ = fs.Parse(args[1:])
		return runTUI(ctx, *sock)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}
```

Why `run()` returning `error` instead of calling `log.Fatal` everywhere: deferred cleanups run, exit code is decided in one place, and `run` is testable.

**Alternative:** `spf13/cobra` gives completions, nested help, and is common in industry. Either is fine; pick one and justify it in the README.

## Version info

GoReleaser injects these via `-ldflags`:

```go
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)
```

Also see `runtime/debug.ReadBuildInfo()` — gives VCS revision even for `go install` builds.

## Best practices

- `main` is wiring only: build config → construct dependencies → call `Run(ctx)`.
- No package-level mutable state (no global DB handle, no global logger mutation outside `main`).
- Accept interfaces, return concrete types. Declare small interfaces in the consuming package:
  ```go
  // in internal/poller
  type SpotifyAPI interface {
      CurrentlyPlaying(ctx context.Context) (*spotify.Playing, error)
      RecentlyPlayed(ctx context.Context, after time.Time) ([]spotify.PlayHistory, error)
  }
  ```
- Package names: short, lowercase, no `utils`/`common`/`helpers`.
- Errors: wrap with context `fmt.Errorf("fetch recently played: %w", err)`; inspect with `errors.Is/As`.

## Pitfalls

- Putting logic in `main` (untestable).
- Huge `models` package that everything imports — creates coupling. Keep DTOs near where they are used (spotify DTOs in `spotify`, DB rows in `store`, wire types in `gen`), and convert at boundaries.
- Import cycles: usually a sign an interface should move to the consumer.

## Further reading

- https://go.dev/doc/modules/layout
- https://go.dev/doc/effective_go
- https://google.github.io/styleguide/go/
