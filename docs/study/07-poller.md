# 07 — Poller design: fast loop, slow loop, change detection, idempotent writes

## Responsibilities

| Loop | Interval | Calls | Writes DB? | Publishes events? |
|------|----------|-------|-----------|-------------------|
| fast | ~5 s | `GET /me/player/currently-playing` | **no** | yes, only on change |
| slow | ~3 min | `GET /me/player/recently-played?after=…` | **yes — the only writer of `plays`** | optional "history updated" event |

Why split them:
- "Currently playing" is a live view: lossy, high frequency, no persistence needed.
- "Recently played" is Spotify's authoritative record of completed plays, with exact `played_at` timestamps. Deriving history from the fast loop (guessing when a song "counted") would be wrong and racy.
- **Single writer** for a table = no write conflicts, simple reasoning, and the fast loop can never corrupt history.

## Change detection (fast loop)

Emit an event only when something meaningful changed:

```go
type NowPlaying struct {
	TrackID    string
	IsPlaying  bool
	ProgressMs int
	DurationMs int
	FetchedAt  time.Time
}

// changed reports whether b differs meaningfully from a.
func changed(a, b *NowPlaying, interval time.Duration) bool {
	if (a == nil) != (b == nil) {
		return true // started or stopped
	}
	if a == nil {
		return false
	}
	if a.TrackID != b.TrackID || a.IsPlaying != b.IsPlaying {
		return true
	}
	// Detect seeks: progress moved by an amount inconsistent with wall-clock time.
	if b.IsPlaying {
		expected := a.ProgressMs + int(b.FetchedAt.Sub(a.FetchedAt).Milliseconds())
		if abs(b.ProgressMs-expected) > 3000 {
			return true
		}
	}
	return false
}
```

The TUI can interpolate progress locally between events (it knows `ProgressMs`, `FetchedAt`, `IsPlaying`), so you don't need to emit every 5 s just to move a progress bar.

## Fast loop

```go
type Poller struct {
	api      SpotifyAPI
	store    PlayStore
	pub      Publisher
	log      *slog.Logger
	fastEvery, slowEvery time.Duration
	now      func() time.Time
}

func (p *Poller) RunFast(ctx context.Context) error {
	var last *NowPlaying
	t := time.NewTicker(p.fastEvery)
	defer t.Stop()

	for {
		cur, err := p.fetchNowPlaying(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			// Transient errors must not kill the daemon. Log and keep going;
			// the client already retried.
			p.log.Warn("poll currently-playing", "err", err)
		case changed(last, cur, p.fastEvery):
			p.pub.Publish(Event{NowPlaying: cur})
			last = cur
		default:
			last = cur
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p *Poller) fetchNowPlaying(ctx context.Context) (*NowPlaying, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pl, err := p.api.CurrentlyPlaying(ctx)
	if errors.Is(err, spotify.ErrNoContent) {
		return nil, nil // nothing playing
	}
	if err != nil {
		return nil, err
	}
	return toNowPlaying(pl, p.now()), nil
}
```

Polling immediately on start (before the first tick) gives the TUI data right away.

**Adaptive interval (nice touch):** poll every 5 s while playing, every 30 s when stopped/paused. Use `t.Reset(d)` when state changes.

## Slow loop — idempotent ingestion

```go
func (p *Poller) RunSlow(ctx context.Context) error {
	t := time.NewTicker(p.slowEvery)
	defer t.Stop()
	for {
		if err := p.ingest(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("ingest recently-played", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p *Poller) ingest(ctx context.Context) error {
	cursor, err := p.store.LatestPlayedAt(ctx) // zero time if empty
	if err != nil {
		return err
	}
	items, err := p.api.RecentlyPlayed(ctx, cursor)
	if err != nil {
		return err
	}
	inserted, err := p.store.InsertPlays(ctx, items) // one transaction, ON CONFLICT DO NOTHING
	if err != nil {
		return err
	}
	if inserted > 0 {
		p.log.Info("ingested plays", "new", inserted)
		p.pub.Publish(Event{HistoryUpdated: &HistoryUpdated{New: inserted}})
	}
	return nil
}
```

SQL (see 09/11):

```sql
-- name: InsertPlay :execrows
INSERT INTO plays (played_at_ms, track_id, track_name, artist_names, album_name, duration_ms, context_uri)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (played_at_ms) DO NOTHING;
```

`:execrows` returns affected rows → 0 means duplicate, 1 means new. Sum them for `inserted`.

### Why `ON CONFLICT DO NOTHING` + cursor

- The cursor (`after = max(played_at)`) minimises how much you fetch.
- The unique constraint guarantees correctness even if the cursor is off-by-one, you crash between fetch and commit, the clock is weird, or you run `ingest` twice. **The DB constraint is the source of truth; the cursor is an optimisation.**
- Ingestion is therefore **idempotent** — safe to retry blindly. Say this explicitly in your README; it's exactly the kind of reasoning interviewers want.

Is `played_at` alone a safe key? A single user can't finish two tracks at the same millisecond, so yes for a single-account daemon. If you ever support multiple accounts, make it `UNIQUE(user_id, played_at_ms)`.

## Testing the poller

- Fake `SpotifyAPI` returning scripted responses; fake `Publisher` recording events.
- Test `changed()` table-driven: same track, track change, pause, resume, seek, stop.
- Test ingest idempotency against a real in-memory SQLite: run `ingest` twice with overlapping data → row count unchanged second time.
- Test loop timing with `testing/synctest` so a 3-minute ticker runs instantly (see 18).
- Test that `RunFast` returns promptly when ctx is cancelled mid-request.

## Pitfalls

- Treating 204 as an error → spammy logs every 5 s while idle.
- Killing the whole errgroup on a transient Spotify error. Poll loops should log and continue; only return on ctx cancellation or unrecoverable errors (e.g. refresh token revoked).
- Storing `played_at` as a formatted string with inconsistent precision → duplicates that the unique index doesn't catch. Normalise to integer UTC milliseconds.
- Fast loop writing to DB "to be helpful" — breaks the single-writer invariant.
