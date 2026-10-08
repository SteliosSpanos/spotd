package poller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/SteliosSpanos/spotd/internal/events"
	"github.com/SteliosSpanos/spotd/internal/spotify"
)

type SpotifyAPI interface {
	CurrentlyPlaying(ctx context.Context) (*spotify.Playing, error)
	RecentlyPlayed(ctx context.Context, after time.Time) ([]spotify.PlayHistory, error)
}

type PlayStore interface {
	LatestPlayedAt(ctx context.Context) (time.Time, error)
	// InsertPlays must be idempotent and returns how many rows were new
	InsertPlays(ctx context.Context, items []spotify.PlayHistory) (int64, error)
}

type Publisher interface {
	Publish(events.Event)
}

func changed(a, b *events.NowPlaying) bool {
	if (a == nil) != (b == nil) {
		return true
	}

	if a == nil {
		return false
	}

	if a.TrackID != b.TrackID || a.IsPlaying != b.IsPlaying {
		return true
	}

	// If the song is playing we expect the new progress to equal the old progress + the time that has passed since we last checked
	if b.IsPlaying {
		expected := a.ProgressMs + int(b.FetchedAt.Sub(a.FetchedAt).Milliseconds())
		// If reality differs from our expectation by more than 3 secs, it means the user manually dragged the progress bar
		if abs(b.ProgressMs-expected) > 3000 {
			return true
		}
	}

	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}

	return n
}

type Poller struct {
	api                  SpotifyAPI
	store                PlayStore
	pub                  Publisher
	log                  *slog.Logger
	fastEvery, slowEvery time.Duration
	now                  func() time.Time // Allows injecting a fake clock for testing
}

func New(api SpotifyAPI, store PlayStore, pub Publisher, l *slog.Logger) *Poller {
	return &Poller{
		api:       api,
		store:     store,
		pub:       pub,
		log:       l,
		fastEvery: 5 * time.Second,
		slowEvery: 3 * time.Minute,
		now:       time.Now,
	}
}

func (p *Poller) RunFast(ctx context.Context) error {
	var last *events.NowPlaying
	t := time.NewTicker(p.fastEvery)
	defer t.Stop()

	for {
		cur, err := p.fetchNowPlaying(ctx)

		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			// Transient errors must not kill the daemon, the client already retried
			p.log.Warn("poll currently-playing", "err", err)
		case changed(last, cur):
			p.pub.Publish(events.Event{NowPlaying: cur})
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

func (p *Poller) fetchNowPlaying(ctx context.Context) (*events.NowPlaying, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pl, err := p.api.CurrentlyPlaying(ctx)
	if errors.Is(err, spotify.ErrNoContent) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return toNowPlaying(pl, p.now()), nil
}

// toNowPlaying returns nil when there is no item (e.g. an ad), which is treated as nothing playing
func toNowPlaying(pl *spotify.Playing, fetchedAt time.Time) *events.NowPlaying {
	if pl == nil || pl.Item == nil {
		return nil
	}

	// Local files have a null id, fall back to the URI so two of them still differ
	id := pl.Item.ID
	if id == "" {
		id = pl.Item.URI
	}

	artists := make([]string, 0, len(pl.Item.Artists))
	for _, a := range pl.Item.Artists {
		artists = append(artists, a.Name)
	}

	return &events.NowPlaying{
		TrackID: id,
		Title: pl.Item.Name,
		Artists: artists,
		Album: pl.Item.Album.Name,
		IsPlaying: pl.IsPlaying,
		ProgressMs: pl.ProgressMs,
		DurationMs: pl.Item.DurationMs,
		FetchedAt fetchedAt,
	}
}

// RunSlow ingests recently-played into the store. It is the only writer of plays and returns only when ctx is done.
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

// ingest is idempotent: the cursor only limits how much is fetched,
// the store's unique constraint is what prevents duplicates
func (p *Poller) ingest(ctx context.Context) error {
	cursor, err := p.store.LatestPlayedAt(ctx) // Ask the database when was the exact timestamp of the last song we saved
	if err != nil {
		return err
	}

	// Ask spotify to give us all songs played after this timestamp
	items, err := p.api.RecentlyPlayed(ctx, cursor)
	if err != nil {
		return err
	}

	// Save them to database and the database uses a unique constraint to ensure we never save the exact same song twice
	inserted, err := p.store.InsertPlays(ctx, items)
	if err != nil {
		return err
	}

	// If we found new songs, tell the app so it can update its stats
	if inserted > 0 {
		p.log.Info("ingested plays", "new", inserted)
		p.pub.Publish(events.Event{HistoryUpdated: &events.HistoryUpdated{New: inserted}})
	}

	return nil
}
