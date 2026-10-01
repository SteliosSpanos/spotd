# spotd study guide

Study notes for every topic the `spotd` architecture touches. Each file covers:
**concepts → idiomatic examples → best practices → pitfalls → how it maps to spotd → further reading**.

> Code snippets are illustrative and written against the libraries as of late 2026.
> Always confirm signatures against the current docs (pkg.go.dev / project READMEs) before copying.

## Suggested study order

Order follows the build order: you can build and test each layer before the next one exists.

| # | File | Component it unlocks |
|---|------|----------------------|
| 01 | [Project layout & CLI subcommands](01-project-layout-and-cli.md) | repo skeleton, `spotd <cmd>` |
| 02 | [OAuth 2.0 + PKCE](02-oauth2-pkce.md) | `spotd login` |
| 03 | [OS keyring](03-keyring.md) | refresh-token storage |
| 04 | [Resilient HTTP client](04-http-client-resilience.md) | Spotify client: retries, backoff, 429, token refresh |
| 05 | [Spotify Web API](05-spotify-web-api.md) | endpoints, scopes, quirks, Feb-2026 changes |
| 06 | [Concurrency fundamentals](06-concurrency-fundamentals.md) | goroutines, channels, context, errgroup, race detector |
| 07 | [Poller design](07-poller.md) | fast + slow loops, change detection, idempotent writes |
| 08 | [Broadcaster (pub/sub)](08-broadcaster.md) | non-blocking fan-out |
| 09 | [SQLite with modernc.org/sqlite](09-sqlite-modernc.md) | persistence, WAL, single writer |
| 10 | [goose migrations](10-goose-migrations.md) | schema evolution |
| 11 | [sqlc](11-sqlc.md) | type-safe queries |
| 12 | [Protobuf + buf](12-protobuf-buf.md) | API contract, codegen, lint, breaking checks |
| 13 | [gRPC over a Unix socket](13-grpc-unix-socket.md) | unary + server-streaming RPCs |
| 14 | [Graceful shutdown](14-graceful-shutdown.md) | signals, context, errgroup wiring |
| 15 | [Structured logging with slog](15-slog.md) | observability |
| 16 | [Bubble Tea TUI (v2)](16-bubbletea-tui.md) | `spotd tui` |
| 17 | [Background jobs with progress](17-background-jobs.md) | playlist dedupe/sort/backup/restore |
| 18 | [Testing strategy](18-testing.md) | unit, integration, race, fuzz, synctest, golden |
| 19 | [golangci-lint](19-golangci-lint.md) | static analysis |
| 20 | [GitHub Actions CI](20-github-actions.md) | CI pipeline |
| 21 | [GoReleaser](21-goreleaser.md) | releases |
| 22 | [Config, paths & security checklist](22-config-paths-security.md) | XDG dirs, permissions, threat model |

## The architecture on one page

```
             ┌──────────── spotd login ────────────┐
             │ PKCE flow, 127.0.0.1:8080/callback  │
             │ refresh token ──► OS keyring         │
             └─────────────────────────────────────┘

┌───────────────────────── spotd daemon ─────────────────────────┐
│                                                                │
│  keyring ─► TokenSource (persists rotated refresh tokens)      │
│                 │                                              │
│          Spotify client (retry, backoff+jitter, 429)           │
│            ▲             ▲                 ▲                   │
│   fast loop (5s)   slow loop (3m)    command handlers          │
│   currently-playing recently-played  play/pause/skip/queue     │
│        │                 │                                     │
│        ▼                 ▼                                     │
│   Broadcaster      SQLite (ONLY writer of plays,               │
│   (fan-out)        INSERT … ON CONFLICT DO NOTHING)            │
│        │                 │                                     │
│        └──── gRPC server on unix socket ────┘                  │
│              unary: commands + stats                           │
│              server-stream: live events, job progress          │
│                                                                │
│  main: signal.NotifyContext + errgroup ─► graceful shutdown    │
└────────────────────────────────────────────────────────────────┘
                           ▲
                           │ gRPC (unix://…/spotd.sock)
┌──────────── spotd tui (Bubble Tea) ────────────┐
│ tabs: Now playing │ Stats │ Playlists (jobs)   │
└────────────────────────────────────────────────┘
```

## What reviewers of a CV project look for

1. **Clear boundaries.** Packages with one job; interfaces defined where they are consumed.
2. **Correct concurrency.** No goroutine leaks, every goroutine has an owner and an exit path, `go test -race` passes.
3. **Failure handling.** Timeouts on every network call, retries only for retryable errors, idempotent writes.
4. **Tests that prove the hard parts.** Broadcaster under a slow subscriber, poller dedupe, retry on 429, graceful shutdown.
5. **Tooling.** Lint, CI, reproducible codegen (`buf generate`, `sqlc generate` checked in CI), tagged releases.
6. **A README with an architecture diagram and the trade-offs you made.**
