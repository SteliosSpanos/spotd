# 15 — Structured logging with log/slog

## Concepts

- `log/slog` (stdlib since Go 1.21): structured key/value logs, levels, pluggable handlers.
- **Logger** = front end you call. **Handler** = back end that formats/filters/writes (`TextHandler`, `JSONHandler`, or custom).
- Attributes: `slog.String`, `slog.Int`, `slog.Duration`, `slog.Any`, `slog.Group`.

## Setup in main

```go
func newLogger(format string, level *slog.LevelVar, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     level,       // LevelVar can be changed at runtime
		AddSource: false,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case "access_token", "refresh_token", "code", "authorization":
				return slog.String(a.Key, "REDACTED")
			}
			return a
		},
	}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}
```

- Daemon: log to **stderr** (systemd/journald and launchd capture it). JSON in production, text for dev (`--log-format`).
- TUI: **never log to stdout/stderr** — it corrupts the screen. Log to a file (`$XDG_STATE_HOME/spotd/tui.log`) or discard.

## Passing loggers

- Inject `*slog.Logger` into constructors; don't call `slog.Default()` deep inside packages.
- Add component context once: `log.With("component", "poller")`.
- Tests: `slog.New(slog.DiscardHandler)` (Go 1.24+), or a handler writing to `t.Output()`/a buffer to assert on logs.

## Type-safe redaction with LogValuer

```go
type Secret string

func (Secret) LogValue() slog.Value { return slog.StringValue("REDACTED") }

log.Info("loaded token", "refresh_token", Secret(rt)) // prints REDACTED
```

Use this type for tokens everywhere — safer than relying only on `ReplaceAttr` key names.

## Performance-friendly calls

```go
log.LogAttrs(ctx, slog.LevelDebug, "poll",
	slog.String("track", id),
	slog.Duration("latency", d),
)
```

`LogAttrs` avoids `any` boxing; disabled levels cost almost nothing. For expensive values, check `log.Enabled(ctx, slog.LevelDebug)` first or use a `LogValuer` (evaluated lazily).

## What to log in spotd

| Event | Level | Attributes |
|-------|-------|------------|
| daemon start/stop | Info | version, socket, db path |
| migration applied | Info | version, duration |
| track changed | Debug | track id |
| plays ingested | Info | new count |
| Spotify retry | Warn | method, path, status, attempt, wait |
| token refresh failed | Error | error (no token!) |
| RPC served | Info/Debug | method, code, duration |
| panic recovered | Error | method, stack |

## Best practices

- Messages are constant strings; variable data goes in attributes (greppable, queryable).
- Consistent keys: `err` for errors, `dur` for durations, `component`.
- Log an error **once**, where it's handled — not at every layer it passes through (wrap with `%w` instead).
- Runtime log level change via `LevelVar` (e.g. on SIGUSR1 or an admin RPC) — nice operational touch.

## Pitfalls

- Odd number of key/value args → `!BADKEY`. `go vet` (the `slog` analyzer) catches this — keep vet in CI.
- Logging full Spotify responses/URLs that contain codes or tokens.
- Logging inside hot loops at Info → noise every 5 s.

## Further reading

- https://go.dev/blog/slog
- https://pkg.go.dev/log/slog
- https://github.com/golang/example/tree/master/slog-handler-guide
