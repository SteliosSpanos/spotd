# 13 — gRPC over a Unix domain socket

## Concepts

- **gRPC** = HTTP/2 + protobuf + generated stubs. RPC kinds: unary, server-streaming, client-streaming, bidi. spotd uses unary (commands, stats) and server-streaming (events, job progress).
- **Unix domain socket (UDS)**: local-only IPC through a filesystem path. No TCP port, no network exposure, access controlled by **file permissions**. Ideal for daemon ↔ local client.
- Status codes (`google.golang.org/grpc/codes`) are the error contract. Map domain errors deliberately.

## Socket location & permissions

- Linux: `$XDG_RUNTIME_DIR/spotd/spotd.sock` (`/run/user/<uid>`, tmpfs, user-only, cleaned on logout).
- macOS: `~/Library/Application Support/spotd/spotd.sock` or `os.TempDir()`-based fallback.
- Create the parent dir with `0700` → only your user can even reach the socket. Then `chmod 0600` the socket.
- **Path length limit**: `sun_path` is ~108 bytes on Linux, 104 on macOS. Long `t.TempDir()` paths in tests can exceed it.

## Server

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/<you>/spotd/gen/spotd/v1"
)

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Stale socket from a crashed daemon? If nobody answers, remove it.
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("daemon already running on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func NewGRPCServer(log *slog.Logger, player pb.PlayerServiceServer, stats pb.StatsServiceServer) *grpc.Server {
	s := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoverUnary(log), logUnary(log)),
		grpc.ChainStreamInterceptor(recoverStream(log), logStream(log)),
	)
	pb.RegisterPlayerServiceServer(s, player)
	pb.RegisterStatsServiceServer(s, stats)
	healthpb.RegisterHealthServer(s, health.NewServer())
	return s
}
```

Implementing a service — embed the generated `Unimplemented…Server` **by value** (forward compatibility; required by recent protoc-gen-go-grpc):

```go
type Player struct {
	pb.UnimplementedPlayerServiceServer
	api SpotifyAPI
	bus *broadcast.Broadcaster[poller.Event]
}

func (p *Player) Pause(ctx context.Context, _ *pb.PauseRequest) (*pb.PauseResponse, error) {
	if err := p.api.Pause(ctx); err != nil {
		return nil, toStatus(err)
	}
	return &pb.PauseResponse{}, nil
}
```

Mapping errors to status codes:

```go
func toStatus(err error) error {
	var apiErr *spotify.APIError
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "spotify timed out")
	case errors.Is(err, auth.ErrNotLoggedIn):
		return status.Error(codes.Unauthenticated, "run `spotd login`")
	case errors.As(err, &apiErr) && apiErr.Reason == "NO_ACTIVE_DEVICE":
		return status.Error(codes.FailedPrecondition, "no active Spotify device")
	case errors.As(err, &apiErr) && apiErr.Status == 429:
		return status.Error(codes.ResourceExhausted, "rate limited by Spotify")
	case errors.As(err, &apiErr) && apiErr.Status == 403:
		return status.Error(codes.PermissionDenied, apiErr.Message)
	default:
		return status.Error(codes.Internal, "internal error") // details go to logs, not clients
	}
}
```

Validate inputs at the edge: `QueueRequest.uri` must match `spotify:track:[A-Za-z0-9]{22}` → `codes.InvalidArgument` otherwise. (Consider `buf.build/bufbuild/protovalidate` for declarative validation.)

## Server-streaming handler

Modern generated code uses generic stream types:

```go
func (p *Player) WatchEvents(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.WatchEventsResponse]) error {
	ch, cancel := p.bus.Subscribe()
	defer cancel()
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return status.Error(codes.Unavailable, "event stream closed; reconnect")
			}
			if err := stream.Send(toProto(ev)); err != nil {
				return err
			}
		}
	}
}
```

## Interceptors (middleware)

```go
func logUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		log.LogAttrs(ctx, slog.LevelInfo, "rpc",
			slog.String("method", info.FullMethod),
			slog.String("code", status.Code(err).String()),
			slog.Duration("dur", time.Since(start)))
		return resp, err
	}
}

func recoverUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in handler", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return h(ctx, req)
	}
}
```

`github.com/grpc-ecosystem/go-grpc-middleware/v2` has ready-made logging (slog adapter) and recovery interceptors.

## Client (TUI side)

```go
conn, err := grpc.NewClient("unix://"+sockPath, // e.g. unix:///run/user/1000/spotd/spotd.sock
	grpc.WithTransportCredentials(insecure.NewCredentials()), // OK: local socket, protected by file perms
)
if err != nil { return err }
defer conn.Close()

player := pb.NewPlayerServiceClient(conn)

ctx, cancel := context.WithTimeout(ctx, 5*time.Second) // deadline on every unary call
defer cancel()
_, err = player.Pause(ctx, &pb.PauseRequest{})
if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition {
	// show "open Spotify on a device"
}
```

- `grpc.NewClient` is lazy (no I/O until first RPC). `grpc.Dial`/`DialContext` are deprecated.
- Reading a stream:

```go
stream, err := player.WatchEvents(ctx, &pb.WatchEventsRequest{})
if err != nil { return err }
for {
	msg, err := stream.Recv()
	if errors.Is(err, io.EOF) { return nil }
	if err != nil { return err } // reconnect with backoff in the TUI
	handle(msg)
}
```

Do **not** put a short timeout on stream contexts — cancel them explicitly when the TUI exits.

## Shutdown interaction

`GracefulStop()` waits for **all** RPCs, including long-lived streams, to finish. Streams only finish if their handlers return. So during shutdown:
1. Cancel the root context / close the broadcaster → stream handlers see `!ok` / `ctx.Done()` and return.
2. Call `GracefulStop()` with a fallback timer that calls `Stop()`.

See 14 for the full wiring.

## Testing

Use a real socket in a short temp dir, or `bufconn` (in-memory listener):

```go
func newTestClient(t *testing.T, srv *grpc.Server) *grpc.ClientConn {
	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { conn.Close() })
	return conn
}
```

Test: status code mapping, stream receives published events, stream ends when client cancels (no goroutine leak), invalid input → `InvalidArgument`.

## Best practices

- Deadlines on every unary client call.
- Never return raw internal error strings to clients.
- Health service registered; `spotd tui` can show "daemon not running" nicely by checking it.
- Optional: verify peer UID with `SO_PEERCRED` (Linux) via a custom `credentials.TransportCredentials` — defense in depth beyond file perms.

## Pitfalls

- Stale socket file → "address already in use" after a crash.
- Socket in a world-readable dir with default perms → any local user can control your Spotify.
- `GracefulStop` hanging forever on open streams.
- Returning `nil, nil` from a unary handler → client gets an error; always return a non-nil response.

## Further reading

- https://grpc.io/docs/languages/go/basics/
- https://pkg.go.dev/google.golang.org/grpc
- https://grpc.io/docs/guides/status-codes/
- https://github.com/grpc/grpc-go/blob/master/Documentation/anti-patterns.md
