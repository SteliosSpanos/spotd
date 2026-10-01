# 06 — Go concurrency fundamentals

The poller, broadcaster, gRPC streams, background jobs and shutdown all rest on these primitives. Get them solid first.

## Goroutines

- Cheap (KB-sized stacks), scheduled by the runtime onto OS threads.
- **Rule: every goroutine must have an owner who knows how it stops.** A goroutine that can block forever is a leak.
- Ask for each `go` statement: *What makes it exit? Who waits for it?*

## Channels

| Operation | nil channel | open channel | closed channel |
|-----------|-------------|--------------|----------------|
| send | blocks forever | blocks until received / buffer space | **panic** |
| receive | blocks forever | blocks until value | zero value, `ok=false` immediately |
| close | panic | ok | **panic** |

Rules of thumb:
- **Only the sender closes**, and only when no more sends can happen.
- Closing is a broadcast signal ("done") — every receiver sees it.
- Unbuffered = synchronization point. Buffered = decoupling with bounded slack. A buffer is not a fix for a design that can deadlock.
- `for v := range ch` ends when `ch` is closed.

## select

```go
select {
case ev := <-events:
	handle(ev)
case <-ctx.Done():
	return ctx.Err()
}
```

Non-blocking send (key for the broadcaster):

```go
select {
case ch <- ev:
default:
	// receiver not ready — drop or count
}
```

Nil-channel trick: setting a channel variable to `nil` disables that `select` case.

## context.Context

- Carries cancellation, deadlines, and request-scoped values down a call tree.
- First parameter, named `ctx`. Never store it in a struct (exception: long-lived objects whose lifetime equals the context, documented).
- Always `defer cancel()` after `WithCancel/WithTimeout`.
- `context.WithoutCancel(ctx)` for work that must finish even if the parent is cancelled (e.g. flush on shutdown with its own timeout).
- `context.AfterFunc(ctx, f)` runs `f` when ctx is done — useful to close a resource that doesn't take a ctx.
- `context.Cause(ctx)` + `WithCancelCause` let you record *why* something was cancelled.

## errgroup (golang.org/x/sync/errgroup)

Runs a group of goroutines; the first error cancels the shared context; `Wait` returns that first error.

```go
g, ctx := errgroup.WithContext(ctx)

g.Go(func() error { return poller.RunFast(ctx) })
g.Go(func() error { return poller.RunSlow(ctx) })
g.Go(func() error { return grpcServer.Run(ctx) })

if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
	return err
}
```

`g.SetLimit(n)` bounds concurrency — useful for fetching many playlists in parallel without hammering the API.

## sync package

- `sync.Mutex` / `RWMutex`: protect shared state. Keep critical sections small; never call out to unknown code (callbacks, channel sends that can block) while holding a lock.
- `sync.WaitGroup`: wait for N goroutines. Go 1.25 added `wg.Go(func(){...})` which does `Add(1)`/`Done()` for you.
- `sync.Once` / `sync.OnceFunc` / `sync.OnceValue`: one-time init, idempotent close.
- `sync/atomic` typed values: `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]` — good for counters (dropped events) and snapshots.

## Tickers and timers

```go
t := time.NewTicker(5 * time.Second)
defer t.Stop()
for {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		tick(ctx)
	}
}
```

- Since Go 1.23 unreferenced timers/tickers are garbage-collected and `Reset`/`Stop` semantics are simpler, but still `defer t.Stop()` for clarity.
- Don't use `time.After` in a hot loop for timeouts on every iteration if you can reuse a timer (allocation and readability).
- A ticker drops ticks if the receiver is slow — that is what you want for polling.

## Patterns you'll use

**Worker owning state (actor):** one goroutine owns a value; others talk to it via channels. Removes locks entirely. Alternative to a mutex for the broadcaster.

**Fan-out with bounded workers:**
```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(4)
for _, id := range playlistIDs {
	g.Go(func() error { return backup(ctx, id) }) // Go 1.22+: loop var is per-iteration
}
return g.Wait()
```

**Pipeline stages** connected by channels, each stage closing its output when its input closes.

## The race detector

```
go test -race ./...
```

- Detects unsynchronized concurrent access *that actually happens during the run*. Write tests that exercise concurrency (many publishers + subscribers).
- Run it in CI on every push. It's CPU-heavy but catches real bugs.
- A race is a bug even if "it works": the memory model gives no guarantees.

## Goroutine leak detection

- `go.uber.org/goleak`: `defer goleak.VerifyNone(t)` in tests, or `goleak.VerifyTestMain(m)`.
- `testing/synctest` (Go 1.25+) also reports goroutines blocked forever inside a bubble as a deadlock.

## Best practices checklist

- [ ] Every goroutine exits on `ctx.Done()` or channel close.
- [ ] Channels are closed by their only sender.
- [ ] No blocking operations while holding a mutex.
- [ ] `go test -race` green.
- [ ] `goleak` in tests for packages that spawn goroutines.
- [ ] Prefer simple: a mutex around a map is often clearer than a channel-based actor.

## Further reading

- https://go.dev/ref/mem (memory model)
- https://go.dev/blog/pipelines
- https://go.dev/blog/context
- "Concurrency in Go" — Katherine Cox-Buday
- https://pkg.go.dev/golang.org/x/sync/errgroup
