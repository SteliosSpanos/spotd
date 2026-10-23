# spotd

A Spotify companion daemon written in Go. `spotd` runs in the background, watches what you are
listening to, keeps a local SQLite history of every play, and exposes playback control and
listening statistics over gRPC on a Unix socket.

> **Status:** early development. The daemon, login flow, poller, history store and the
> Player/Stats gRPC services work today. The playlist job engine is wired in through `PlaylistService`,
> and the TUI is not written yet. See [Roadmap](#roadmap).

## Table of contents

- [Features](#features)
- [Requirements](#requirements)
- [Getting started](#getting-started)
  - [1. Create a Spotify app](#1-create-a-spotify-app)
  - [2. Build](#2-build)
  - [3. Log in](#3-log-in)
  - [4. Run the daemon](#4-run-the-daemon)
  - [5. Talk to the daemon](#5-talk-to-the-daemon)
- [CLI reference](#cli-reference)
- [Files and paths](#files-and-paths)
- [Architecture](#architecture)
  - [Overview](#overview)
  - [Package layout](#package-layout)
  - [Authentication flow](#authentication-flow)
  - [Daemon lifecycle](#daemon-lifecycle)
  - [Poller](#poller)
  - [Event broadcaster](#event-broadcaster)
  - [Spotify HTTP client](#spotify-http-client)
  - [Storage](#storage)
  - [gRPC API](#grpc-api)
  - [Playlist jobs](#playlist-jobs)
- [Security](#security)
- [Development](#development)
- [Roadmap](#roadmap)

## Features

- **OAuth 2.0 + PKCE login** with no client secret. The refresh token lives in the OS keyring,
  never on disk.
- **Live "now playing"** polled every 5 seconds, with change detection that also catches seeks.
- **Complete play history** ingested every 3 minutes from Spotify's recently-played endpoint
  into SQLite. Ingestion is idempotent, so restarts never duplicate plays.
- **Listening stats**: top artists, top tracks and listening time per day, over the last 7 days,
  the last 30 days or all time.
- **Playback control**: play, pause, skip and queue.
- **gRPC over a Unix socket**, with unary RPCs for commands and stats and a server stream for
  live events.
- **Resilient Spotify client**: exponential backoff with jitter, `Retry-After` support, and no
  retries for non-idempotent requests.
- **Graceful shutdown** on `SIGINT`/`SIGTERM`.

## Requirements

- Go **1.26+**
- A Spotify account. Playback control needs **Spotify Premium** and an active device.
- An OS keyring: Secret Service (GNOME Keyring or KWallet) on Linux, Keychain on macOS, or
  Credential Manager on Windows.
- Optional, for regenerating code: [`buf`](https://buf.build) and [`sqlc`](https://sqlc.dev).

## Getting started

### 1. Create a Spotify app

1. Open the [Spotify Developer Dashboard](https://developer.spotify.com/dashboard) and create an app.
2. Add this redirect URI **exactly**:
   ```
   http://127.0.0.1:8080/callback
   ```
3. Copy the app's **Client ID**. You don't need a client secret, because spotd uses PKCE.

### 2. Build

```sh
git clone https://github.com/SteliosSpanos/spotd.git
cd spotd
go build -o spotd ./cmd/spotd
```

### 3. Log in

```sh
export SPOTD_CLIENT_ID=<your-client-id>
./spotd login
```

spotd opens the browser (or prints the URL), waits up to 5 minutes for the callback on
`127.0.0.1:8080`, then stores the refresh token in the OS keyring.

### 4. Run the daemon

```sh
./spotd daemon                       # text logs at info level
./spotd daemon -log-level debug -log-format json
```

On startup the daemon applies any pending database migrations, starts both poll loops and
listens on the Unix socket. Press `Ctrl+C` to stop it cleanly.

### 5. Talk to the daemon

There is no first-party client yet, so use [`grpcurl`](https://github.com/fullstorydev/grpcurl)
with the proto file. Server reflection is not enabled.

```sh
SOCK=$XDG_RUNTIME_DIR/spotd/spotd.sock
PROTO="-import-path proto -proto spotd/v1/spotd.proto"

# Live events (now playing, stopped, history updated)
grpcurl -plaintext -unix $PROTO $SOCK spotd.v1.PlayerService/WatchEvents

# Playback
grpcurl -plaintext -unix $PROTO $SOCK spotd.v1.PlayerService/Pause
grpcurl -plaintext -unix $PROTO -d '{"uri":"spotify:track:4cOdK2wGLETKBW3PvgPWqT"}' \
  $SOCK spotd.v1.PlayerService/Play

# Stats
grpcurl -plaintext -unix $PROTO -d '{"range":"TIME_RANGE_LAST_30_DAYS","limit":5}' \
  $SOCK spotd.v1.StatsService/TopArtists

# Health check
grpcurl -plaintext -unix $SOCK grpc.health.v1.Health/Check
```

## CLI reference

```
usage: spotd <command> [flags]
```

| Command   | Description                                         |
|-----------|-----------------------------------------------------|
| `login`   | Authorize spotd with your Spotify account           |
| `logout`  | Remove the stored refresh token from the keyring    |
| `daemon`  | Run the background poller and gRPC server           |
| `version` | Print version, commit and build date                |
| `help`    | Print usage                                         |

**`login` flags**

| Flag         | Default            | Description            |
|--------------|--------------------|------------------------|
| `-client-id` | `$SPOTD_CLIENT_ID` | Spotify app client ID  |

**`daemon` flags**

| Flag          | Default                            | Description                         |
|---------------|------------------------------------|-------------------------------------|
| `-client-id`  | `$SPOTD_CLIENT_ID`                 | Spotify app client ID (required)    |
| `-socket`     | `$XDG_RUNTIME_DIR/spotd/spotd.sock` | Unix socket path                    |
| `-db`         | `$XDG_DATA_HOME/spotd/spotd.db`    | SQLite database path                |
| `-log-level`  | `info`                             | `debug`, `info`, `warn` or `error`  |
| `-log-format` | `text`                             | `text` or `json`                    |

`version`, `commit` and `date` are set at build time through `-ldflags`:

```sh
go build -ldflags "-X main.version=v0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%FT%TZ)" ./cmd/spotd
```

## Files and paths

| What          | Location                                                                 | Permissions |
|---------------|--------------------------------------------------------------------------|-------------|
| Refresh token | OS keyring, service `spotd`, account `spotify-refresh-token`             | OS-managed  |
| Database      | `$XDG_DATA_HOME/spotd/spotd.db`, otherwise `~/.local/share/spotd/spotd.db` (Linux) or `~/Library/Application Support/spotd/spotd.db` (macOS) | dir `0700` |
| Socket        | `$XDG_RUNTIME_DIR/spotd/spotd.sock`, otherwise next to the database       | dir `0700`, socket `0600` |

XDG variables are honoured only when they hold an absolute path.

## Architecture

### Overview

spotd uses a **single-daemon, many-clients** design. The daemon is the only process that talks
to Spotify and the only process that writes to the database. Clients connect over a local Unix
socket.

```
                ┌──────────────────── spotd login ────────────────────┐
                │  PKCE auth code flow, callback on 127.0.0.1:8080    │
                │  refresh token ──────────────► OS keyring           │
                └─────────────────────────────────────────────────────┘

┌──────────────────────────────── spotd daemon ────────────────────────────────┐
│                                                                              │
│   OS keyring ──► TokenSource (auto-refresh, persists rotated refresh token)  │
│                        │                                                     │
│                        ▼                                                     │
│          ┌──────── spotify.Client ─────────┐       api.spotify.com/v1        │
│          │ retries · backoff+jitter · 429  │◄────────────────────────────►   │
│          └───▲──────────────▲──────────▲───┘                                 │
│              │              │          │                                     │
│      ┌───────┴──────┐ ┌─────┴──────┐ ┌─┴──────────────┐                      │
│      │ RunFast (5s) │ │ RunSlow(3m)│ │ PlayerService  │                      │
│      │ currently-   │ │ recently-  │ │ Play/Pause/    │                      │
│      │ playing      │ │ played     │ │ Next/Queue     │                      │
│      └───────┬──────┘ └─────┬──────┘ └───────▲────────┘                      │
│              │ NowPlaying   │ InsertPlays    │                               │
│              │              ▼                │                               │
│              │      ┌──────────────┐  ┌──────┴────────┐                      │
│              │      │   SQLite     │  │ StatsService  │                      │
│              │      │ 1 writer     │◄─┤ (read pool)   │                      │
│              │      │ 4 readers    │  └──────▲────────┘                      │
│              │      └──────┬───────┘         │                               │
│              │             │ HistoryUpdated  │                               │
│              ▼             ▼                 │                               │
│        ┌──────────────────────────┐          │                               │
│        │ events.Broadcaster       │──► WatchEvents (server stream)           │
│        │ fan-out, drop-oldest     │          │                               │
│        └──────────────────────────┘          │                               │
│                                              │                               │
│      gRPC server: recover + status/log interceptors, health service          │
│                                              │                               │
└──────────────────────────────────────────────┼───────────────────────────────┘
                                               │ unix socket (0600)
                                  ┌────────────┴────────────┐
                                  │  clients (grpcurl, TUI) │
                                  └─────────────────────────┘
```

### Package layout

```
spotd/
├── cmd/spotd/            CLI entry point and daemon wiring
│   ├── main.go           subcommand dispatch, flags, logger, signal handling
│   └── daemon.go         builds every component and runs them under an errgroup
├── internal/
│   ├── auth/             OAuth2 PKCE login, persisting TokenSource, keyring store
│   ├── config/           XDG / OS-specific paths for the DB and socket
│   ├── spotify/          resilient HTTP client and typed Web API calls
│   ├── poller/           fast (now playing) and slow (history) loops
│   ├── events/           generic Broadcaster[T] and event types
│   ├── store/            SQLite open/migrate, write path and stats queries
│   │   ├── migrations/   goose SQL migrations (embedded in the binary)
│   │   ├── queries/      sqlc source queries
│   │   └── db/           sqlc-generated code (do not edit)
│   ├── server/           gRPC services, interceptors, error mapping, socket listener
│   └── jobs/             playlist job manager and runners
├── proto/spotd/v1/       protobuf API definition
├── gen/spotd/v1/         buf-generated Go code (do not edit)
└── docs/study/           study notes for each technology used
```

Interfaces are declared **where they are consumed**. For example, `poller.SpotifyAPI`,
`poller.PlayStore`, `server.PlayerAPI`, `server.StatsStore` and `jobs.PlaylistAPI` each list only
the methods their consumer needs. Each package can then be tested with small fakes.

### Authentication flow

```
 spotd login                     Browser                    accounts.spotify.com
     │                              │                               │
     │ generate PKCE verifier and   │                               │
     │ a 32-byte random state       │                               │
     │ listen on 127.0.0.1:8080     │                               │
     │──── open authorize URL ─────►│                               │
     │     (S256 challenge, state)  │──── user approves scopes ────►│
     │                              │◄──── 302 /callback?code&state ─│
     │◄──── GET /callback ──────────│                               │
     │ constant-time state check    │                               │
     │──────────── exchange code + verifier ───────────────────────►│
     │◄─────────── access + refresh token ──────────────────────────│
     │ save refresh token ──► OS keyring                            │
```

Inside the daemon, `auth.NewTokenSource` wraps the oauth2 token source in a `persistingSource`.
When Spotify rotates the refresh token, the new one is written back to the keyring, so the
daemon never needs another browser login. A mutex guards concurrent refreshes.

### Daemon lifecycle

```
runDaemon
  │
  ├─ load refresh token from keyring        (fail fast if logged out)
  ├─ mkdir data dir (0700), open SQLite, run goose migrations
  ├─ build TokenSource ► spotify.Client ► Broadcaster ► Store ► Poller
  ├─ ListenUnix (refuse if another daemon answers, remove stale socket, chmod 0600)
  │
  └─ errgroup ──┬── poller.RunFast
                ├── poller.RunSlow
                ├── grpc.Serve
                └── shutdown watcher:
                       wait for ctx.Done()  (SIGINT / SIGTERM / any goroutine error)
                       bus.Close()          ends every WatchEvents stream
                       GracefulStop()       up to 10s, then Stop()
                       remove socket file
```

`signal.NotifyContext` turns signals into context cancellation. If any goroutine returns an
error, the errgroup cancels the shared context and the rest of the daemon shuts down too. A
`context.Canceled` result counts as a clean exit.

### Poller

| Loop      | Interval | Endpoint                        | Output                                  |
|-----------|----------|---------------------------------|-----------------------------------------|
| `RunFast` | 5 s      | `GET /me/player/currently-playing` | `NowPlaying` event, published only on change |
| `RunSlow` | 3 min    | `GET /me/player/recently-played`   | rows in SQLite + `HistoryUpdated` event      |

**Change detection** (`changed`): an event is published when the track changes, play/pause
toggles, playback starts or stops, or progress drifts more than 3 s from
`last.progress + elapsed`. The drift check catches seeks without publishing on every tick.

**Ingestion** is idempotent. The newest stored `played_at` is only a cursor that limits how much
is fetched. Duplicates are actually prevented by the `plays.played_at_ms` primary key with
`ON CONFLICT DO NOTHING`. Transient errors in either loop are logged and the loop keeps going.
Only context cancellation stops it.

Local files have no Spotify ID, so their URI is used as the ID in both the poller and the store.

### Event broadcaster

`events.Broadcaster[T]` is a generic in-process pub/sub used for live playback events and, in
the jobs package, for per-job progress.

```
                    Publish(v)
 Poller ──────────────┐
                      ▼
            ┌────────────────────┐   last = v if replay(v) (replayed to new subscribers)
            │   Broadcaster[T]   │
            └──┬───────┬───────┬─┘
               ▼       ▼       ▼        buffered chan per subscriber (16)
            [sub A] [sub B] [sub C]     full? drop the oldest event, push the newest
               │       │       │
          WatchEvents streams (one per gRPC client)
```

- **Publishing never blocks.** A slow subscriber loses its oldest unread events and is never
  disconnected, so it can't stall the poller.
- **New subscribers get the last event right away.** A client that connects mid-song sees the
  current track without waiting for the next change.
- **Replay is filterable.** `New` caches every event for late subscribers. `NewWithReplay(bufSize,
  replay)` caches only events for which `replay` returns true, while still delivering all events
  to current subscribers. The daemon's bus uses `events.Event.Replayable`, so a one-off
  `HistoryUpdated` is never replayed to a newly connected `WatchEvents` client and can't replace
  the cached now-playing state. Job progress uses plain `New`.
- The `cancel` returned by `Subscribe` is wrapped in `sync.OnceFunc`, so calling it twice is safe.
  `Close()` closes every subscriber channel.

### Spotify HTTP client

`spotify.Client.do` wraps every request with these rules:

```
 request ──► attach bearer token ──► http.Do (20s timeout)
                                         │
              ┌──────────────────────────┼──────────────────────────────┐
              ▼                          ▼                              ▼
         2xx / 204                  429 Too Many                  5xx / network error
     decode JSON (≤10 MiB)      wait Retry-After (≤2m)         retry ONLY if idempotent
     204 ► ErrNoContent         then retry                     backoff: 500ms·2ⁿ, cap 30s,
                                                               full jitter, max 4 retries
              4xx (other) ──► never retried, returned as *APIError{Status, Message, Reason}
```

- `GET`, `PUT` and `DELETE` are marked idempotent. `POST` calls (`Next`, `Queue`,
  `CreatePlaylist`, `AddItems`) are **never** retried, because a retry could skip twice or queue
  a track twice.
- Context cancellation is never retried, and sleeps are context-aware.
- `paginate` follows Spotify's `next` URLs but refuses any URL outside the API base, so the
  bearer token can't leak to another host.

### Storage

SQLite through the pure-Go driver `modernc.org/sqlite`, so no cgo is needed.

```
           ┌────────── store.DB ──────────┐
 Poller ──►│ W: 1 conn, _txlock=immediate │──┐
           │ R: ≤4 conns, mode=ro         │◄─┼── StatsService
           └──────────────────────────────┘  │
                     WAL journal, busy_timeout=5s, synchronous=NORMAL, foreign_keys=ON
```

There is a **single writer** connection, so SQLite never returns `SQLITE_BUSY` between
writers. A separate read-only pool lets stats queries run while ingestion writes, which WAL
mode allows.

**Schema** (`migrations/00001_init.sql`, every table `STRICT`):

```
 ┌──────────────┐        ┌──────────────────┐        ┌─────────────┐
 │   tracks     │        │  track_artists   │        │   artists   │
 ├──────────────┤        ├──────────────────┤        ├─────────────┤
 │ id        PK │◄───────│ track_id   PK,FK │   ┌───►│ id       PK │
 │ name         │        │ artist_id  PK,FK │───┘    │ name        │
 │ album_name   │        │ position         │        └─────────────┘
 │ duration_ms  │        └──────────────────┘          0 = primary
 └──────▲───────┘
        │
 ┌──────┴───────────────┐
 │       plays          │
 ├──────────────────────┤
 │ played_at_ms  PK     │  ◄── idempotency key
 │ track_id      FK     │      (index plays_track_idx)
 └──────────────────────┘
```

`InsertPlays` runs one transaction per batch. For each item it upserts the track, replaces the
track's artist links, upserts the artists, and inserts the play. It returns how many plays were
new.

- **Migrations:** [goose](https://github.com/pressly/goose), with the SQL files embedded through
  `//go:embed` and applied automatically at daemon start.
- **Queries:** [sqlc](https://sqlc.dev) generates type-safe Go from `store/queries/*.sql` into
  `store/db`.
- **Stats:** `TopArtists` credits every artist on a track. `ListeningByDay` groups by local-time
  date and sums track durations.

### gRPC API

The contract lives in [`proto/spotd/v1/spotd.proto`](proto/spotd/v1/spotd.proto).

| Service           | RPC              | Type          | Notes                                                     |
|-------------------|------------------|---------------|-----------------------------------------------------------|
| `PlayerService`   | `Play`           | unary         | Empty `uri` resumes playback. Otherwise it must match `spotify:track:<22 chars>` |
|                   | `Pause`          | unary         |                                                           |
|                   | `Next`           | unary         |                                                           |
|                   | `Queue`          | unary         | `uri` required and validated                              |
|                   | `WatchEvents`    | server stream | `NowPlaying` \| `Stopped` \| `HistoryUpdated`             |
| `StatsService`    | `TopArtists`     | unary         | `range` + `limit` (default 10, max 100)                   |
|                   | `TopTracks`      | unary         | same                                                      |
|                   | `ListeningByDay` | unary         | `range`                                                   |
| `grpc.health.v1`  | `Check`/`Watch`  | standard      |                                                           |
| `PlaylistService` | —                | implemented   | `ListPlaylists`, `StartJob`, `WatchJob`, `CancelJob`      |

**Interceptor chain** (unary and stream): `recover` → `status/log` → handler.

- `recover` turns a handler panic into `codes.Internal` and logs the stack trace.
- `status/log` writes one structured log line per RPC (method, code, duration) and maps
  domain errors to gRPC codes through `toStatus`:

| Domain error                          | gRPC code            |
|---------------------------------------|----------------------|
| `context.Canceled`                    | `Canceled`           |
| `context.DeadlineExceeded`            | `DeadlineExceeded`   |
| `auth.ErrNotLoggedIn` / Spotify `401` | `Unauthenticated`    |
| Spotify reason `NO_ACTIVE_DEVICE`     | `FailedPrecondition` |
| Spotify `403`                         | `PermissionDenied`   |
| Spotify `429`                         | `ResourceExhausted`  |
| anything else                         | `Internal` (details only in logs) |

### Playlist jobs

> Implemented in `internal/jobs` and exposed through `PlaylistService` (`StartJob`, `WatchJob`, `CancelJob`).

`jobs.Manager` runs long playlist operations inside the daemon, so they keep going even if the
client that started them disconnects.

```
 Start(kind, playlistID, runner)
   │  playlist already busy? ──► ErrBusy
   ▼
 PENDING ──(semaphore, max 2 running)──► RUNNING ──┬──► SUCCEEDED
   │                                               ├──► FAILED
   └────────────── Cancel(id) ─────────────────────┴──► CANCELED
                                                   │
          per-job Broadcaster[Progress] ──► Subscribe(id) (done/total/message)
          finished jobs stay watchable for 10 minutes
```

| Job       | What it does                                                                                  |
|-----------|-----------------------------------------------------------------------------------------------|
| `Backup`  | Saves the playlist to `<backupDir>/<id>-<UTC timestamp>.json` with an atomic write (temp file, fsync, rename) |
| `Restore` | Recreates the **newest** backup as a **new** private playlist `"<name> (restored)"`. It never overwrites the original |
| `Dedupe`  | Backs up first, removes every duplicated URI (snapshot-guarded), then re-adds one copy of each |
| `Sort`    | Backs up first, sorts by primary artist, album, title (case-insensitive), then rewrites the playlist in 100-track chunks. It refuses if the playlist has local or unavailable tracks |

Playlist IDs are checked against base62 before they are used in file paths or glob patterns.
Every destructive job takes a backup first.

## Security

- **No client secret.** PKCE (S256) with a random 32-byte `state`, compared in constant time.
- **Loopback-only callback** on `127.0.0.1:8080`, with a header-read timeout and a 5-minute
  login window.
- **Refresh token only in the OS keyring.** Access tokens live only in memory.
- **Local-only API.** The Unix socket has mode `0600` inside a `0700` directory, and there is
  no TCP listener.
- **Single instance.** The daemon refuses to start if another daemon answers on the socket.
- **Input validation.** Track URIs are matched against a regex, stats `limit` is clamped,
  playlist IDs must be base62, and URLs are path/query-escaped.
- **Token confinement.** Pagination never follows URLs outside `api.spotify.com/v1`.
- **No internal details leak to clients.** Unknown errors become `Internal`, and the details go
  only to the logs.
- **Bounded reads.** Response bodies are capped at 10 MiB.

## Development

```sh
go build ./...            # build
go vet ./...              # static checks
go test -race ./...       # tests (none yet, see Roadmap)

buf lint                  # lint the proto (STANDARD rules)
buf breaking --against '.git#branch=main'
buf generate              # regenerate gen/ (uses protoc-gen-go(-grpc) from `go tool`)
sqlc generate             # regenerate internal/store/db from queries + migrations
```

Generated code (`gen/`, `internal/store/db/`) is committed. Don't edit it by hand: change the
`.proto` or `.sql` source and regenerate.

To add a database change, add a new `internal/store/migrations/0000N_<name>.sql` file with
`-- +goose Up` / `-- +goose Down` sections. The daemon applies it on its next start.
