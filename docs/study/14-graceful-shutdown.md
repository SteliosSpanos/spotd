# 14 — Graceful shutdown with context + errgroup

## Goal

On `SIGINT`/`SIGTERM` (Ctrl-C, `systemctl stop`, `launchctl`):
1. Stop accepting new work.
2. Let in-flight work finish (bounded by a timeout).
3. Stop goroutines in dependency order: producers before consumers' resources go away.
4. Flush/close resources: gRPC server → pollers → broadcaster → DB.
5. Exit 0 on clean shutdown, non-zero if a component failed.

## Building blocks

- `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` → a context cancelled on signal. Call `stop()` after it fires to restore default behaviour (a second Ctrl-C then kills immediately).
- `errgroup.WithContext` → if any component fails, the shared ctx is cancelled and everything else shuts down too.
- Each component's `Run(ctx) error` blocks until ctx is done, then cleans up and returns.

## Wiring the daemon

```go
func runDaemon(parent context.Context, cfg Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close() // runs LAST (after g.Wait returns)

	if err := store.Migrate(ctx, db.W, log); err != nil {
		return err
	}

	ts, err := auth.LoadTokenSource(ctx, cfg, log)
	if err != nil {
		return err
	}
	api := spotify.New(ts, log)
	bus := broadcast.New[poller.Event](16, 50)
	p := poller.New(api, store.New(db), bus, log)

	ln, err := listenUnix(cfg.SocketPath)
	if err != nil {
		return err
	}
	srv := server.NewGRPCServer(log, server.NewPlayer(api, bus), server.NewStats(store.New(db)))

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error { return p.RunFast(gctx) })
	g.Go(func() error { return p.RunSlow(gctx) })

	g.Go(func() error {
		log.Info("grpc listening", "socket", cfg.SocketPath)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})

	// Shutdown coordinator: waits for cancellation, then stops things in order.
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down", "cause", context.Cause(gctx))
		stop() // a second signal now kills the process

		bus.Close() // ends all WatchEvents streams so GracefulStop can complete

		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			log.Warn("graceful stop timed out; forcing")
			srv.Stop()
		}
		_ = os.Remove(cfg.SocketPath)
		return nil
	})

	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		err = nil // normal shutdown
	}
	log.Info("daemon stopped", "err", err)
	return err
}
```

Points to notice:
- `defer db.Close()` runs after `g.Wait()`, i.e. after the pollers (the only DB users) have returned. Order of defers matters.
- `srv.Serve` returns as soon as `GracefulStop`/`Stop` is called, so it doesn't need ctx.
- Poll loops return `ctx.Err()`; we treat `context.Canceled` as success.
- A component returning a *real* error (e.g. socket bind failed) cancels `gctx` → everything else shuts down → `Wait` returns that error → exit code 1.

## Finishing in-flight work

If the slow loop is mid-transaction when ctx is cancelled, `ExecContext` aborts and the tx rolls back. That's fine because ingestion is idempotent — next start re-fetches. If you ever need "finish this write even though we're shutting down":

```go
wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
defer cancel()
```

## Testing shutdown

- Start the daemon in a goroutine with a cancellable ctx and fakes; cancel; assert `run` returns `nil` within N seconds and the socket file is removed.
- Open a `WatchEvents` stream first, then cancel → proves streams don't block `GracefulStop`.
- `goleak.VerifyNone(t)` at the end.
- Manual: `kill -TERM <pid>` and watch logs.

## systemd / launchd notes

- systemd sends SIGTERM, waits `TimeoutStopSec` (default 90 s), then SIGKILL. Your internal timeout should be shorter.
- Ship an example `spotd.service` user unit (`systemctl --user`) in `contrib/` — nice touch for the CV.

## Pitfalls

- Using `os.Exit` or `log.Fatal` inside goroutines → defers never run, socket left behind.
- `GracefulStop` without closing streams → hangs forever.
- Closing the DB while a goroutine still uses it → "sql: database is closed" errors at shutdown.
- Ignoring the second signal → user can't force-quit a stuck daemon.

## Further reading

- https://pkg.go.dev/os/signal#NotifyContext
- https://pkg.go.dev/golang.org/x/sync/errgroup
- https://pkg.go.dev/google.golang.org/grpc#Server.GracefulStop
