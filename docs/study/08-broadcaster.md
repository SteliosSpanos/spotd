# 08 — Broadcaster: race-free, non-blocking pub/sub

## Requirements (from the spec)

1. One publisher (the poller) → many subscribers (gRPC streams, one per TUI).
2. `Publish` must **never block** on a slow subscriber.
3. Laggards get dropped events or are disconnected.
4. Race-free: concurrent `Subscribe`, `Unsubscribe`, `Publish`, `Close`.

## Design decisions

| Question | Choice | Why |
|----------|--------|-----|
| Per-subscriber buffer | small buffered channel (e.g. 16) | absorbs bursts without unbounded memory |
| Full buffer | non-blocking send; count drops; disconnect after N consecutive drops | live state is "latest wins"; a client that's permanently behind should reconnect |
| Who closes subscriber channels | the broadcaster, under its lock | only the sender closes; avoids send-on-closed panic |
| New subscriber initial state | send the last event (snapshot) immediately | TUI shows "now playing" without waiting for the next change |
| Locking | one `sync.Mutex` | sends are non-blocking, so holding the lock during fan-out is safe and simple |

Holding the lock while sending is normally a smell — but here every send is non-blocking (`select … default`), so the critical section is bounded. That's what makes the simple design correct.

## Implementation

```go
package broadcast

import (
	"sync"
)

type Broadcaster[T any] struct {
	mu        sync.Mutex
	subs      map[*sub[T]]struct{}
	last      *T
	closed    bool
	bufSize   int
	maxMisses int
}

type sub[T any] struct {
	ch     chan T
	misses int
}

func New[T any](bufSize, maxMisses int) *Broadcaster[T] {
	return &Broadcaster[T]{subs: make(map[*sub[T]]struct{}), bufSize: bufSize, maxMisses: maxMisses}
}

// Subscribe returns a receive-only channel and an idempotent cancel func.
// The channel is closed when cancel is called, when the subscriber lags
// too far behind, or when the broadcaster closes.
func (b *Broadcaster[T]) Subscribe() (<-chan T, func()) {
	s := &sub[T]{ch: make(chan T, b.bufSize)}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	if b.last != nil {
		s.ch <- *b.last // buffer is empty, cannot block
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	return s.ch, sync.OnceFunc(func() { b.remove(s) })
}

func (b *Broadcaster[T]) remove(s *sub[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; ok { // may already be removed as a laggard
		delete(b.subs, s)
		close(s.ch)
	}
}

// Publish never blocks.
func (b *Broadcaster[T]) Publish(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.last = &v
	for s := range b.subs {
		select {
		case s.ch <- v:
			s.misses = 0
		default:
			s.misses++
			if s.misses >= b.maxMisses {
				delete(b.subs, s) // deleting during range is allowed in Go
				close(s.ch)
			}
		}
	}
}

func (b *Broadcaster[T]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		close(s.ch)
	}
	clear(b.subs)
}
```

### Alternative drop policy: "drop oldest, keep newest"

For live state, the *newest* event is the valuable one. Instead of dropping the new event when the buffer is full, evict one old event and retry:

```go
select {
case s.ch <- v:
default:
	select { case <-s.ch: default: } // drop oldest
	select { case s.ch <- v: default: }
}
```

Caveat: the publisher now *receives* from the subscriber channel, which races with the consumer for which value is dropped — still correct (no blocking, no panic) but harder to reason about. A `chan T` of size 1 with drop-oldest is effectively a "latest value" mailbox. Pick one policy, document it, test it.

### Alternative design: actor goroutine

A single goroutine owns the subscriber map and receives `subscribe`/`unsubscribe`/`publish` messages on channels. No mutex, but `Publish` then has to send to the actor — and that send must itself be non-blocking or buffered. More moving parts; the mutex version is clearer. Mention you considered it.

## Using it from a gRPC stream

```go
func (s *Server) WatchEvents(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.Event]) error {
	ch, cancel := s.bus.Subscribe()
	defer cancel()

	for {
		select {
		case <-stream.Context().Done(): // client went away or server shutting down
			return stream.Context().Err()
		case ev, ok := <-ch:
			if !ok {
				return status.Error(codes.Unavailable, "subscription closed (lagging or shutting down); reconnect")
			}
			if err := stream.Send(toProto(ev)); err != nil {
				return err
			}
		}
	}
}
```

`stream.Send` can block (flow control) — that's fine, it blocks *this* subscriber's goroutine, never the publisher. That is the whole point of the per-subscriber buffer.

## Tests that prove the requirements

```go
func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	b := New[int](1, 3)
	_, cancel := b.Subscribe() // never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			b.Publish(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked")
	}
}

func TestLaggardIsDisconnected(t *testing.T) {
	b := New[int](1, 2)
	ch, _ := b.Subscribe()
	for i := range 5 {
		b.Publish(i)
	}
	// drain: we should see the buffered value then a closed channel
	for range ch {
	}
}

func TestConcurrentUse(t *testing.T) { // run with -race
	b := New[int](4, 10)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ch, cancel := b.Subscribe()
			for range 5 {
				select {
				case <-ch:
				case <-time.After(time.Millisecond):
				}
			}
			cancel()
			cancel() // idempotent
		})
	}
	wg.Go(func() {
		for i := range 10_000 {
			b.Publish(i)
		}
	})
	wg.Wait()
	b.Close()
	b.Close()
}
```

Also: `goleak.VerifyNone(t)`, a test that `Subscribe` after `Close` returns a closed channel, and a test that a new subscriber receives the last event.

## Pitfalls

- Closing a subscriber channel without holding the same lock `Publish` uses → "send on closed channel" panic.
- Unsubscribe func that isn't idempotent → double close panic.
- Blocking send "just with a timeout" → publisher still stalls for the timeout × N slow subscribers.
- Sharing pointers to mutable event structs across subscribers → data race if anyone mutates. Publish values or treat events as immutable.
- Forgetting the snapshot → TUI shows empty until next track change.

## Further reading

- https://go.dev/blog/pipelines
- https://pkg.go.dev/sync#OnceFunc
- https://github.com/uber-go/goleak
