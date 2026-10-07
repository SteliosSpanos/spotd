package events

import (
	"sync"
)

// When the Poller notices a song changed, it doesn't need to know who cares about that change. It just hands the event to this Broadcaster
// The Broadcaster then duplicates and pushes that event to anyone who has subscribed

type Broadcaster[T any] struct {
	mu      sync.Mutex
	subs    map[*sub[T]]struct{} // By using a pointer as the key, the memory address acts as a unique id
	last    *T                   // Caches the most recently published event
	closed  bool
	bufSize int // How many unread events a subscriber can hold before getting full
}

type sub[T any] struct {
	ch chan T
}

func New[T any](bufSize int) *Broadcaster[T] {
	return &Broadcaster[T]{subs: make(map[*sub[T]]struct{}), bufSize: bufSize}
}

// Subscribe gives a listener a channel to receive events on, and a function to hang up (like cancel())
// A subscriber that lags behind is never disconnected, it loses its oldest unread events instead
// In Go maps are not thread-safe so we use mutexes
func (b *Broadcaster[T]) Subscribe() (<-chan T, func()) {
	s := &sub[T]{ch: make(chan T, b.bufSize)}

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}

	if b.last != nil {
		s.ch <- *b.last
	}

	b.subs[s] = struct{}{}
	b.mu.Unlock()

	// OnceFunc guarantees that even if the user calls cancel() 50 times, it only actually runs the remove logic once
	return s.ch, sync.OnceFunc(func() { b.remove(s) })
}

func (b *Broadcaster[T]) remove(s *sub[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
}

// Publish is called by the producer (the Poller)
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
		default:
			// If we hit this default block, the subscriber's channel buffer is completely full

			// We try to forcefully pull the oldest event out of their channel to make room
			select {
			case <-s.ch:
			default:
				// If we couldn't pull it, it means the subscriber literally just pulled it themselves, so the slot is free now
			}

			// Now that there is room, push the newest event into the channel
			select {
			case s.ch <- v:
			default:
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
