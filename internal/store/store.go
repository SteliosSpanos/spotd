package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/SteliosSpanos/spotd/internal/spotify"
	"github.com/SteliosSpanos/spotd/internal/store/db"
)

type Store struct {
	w *sql.DB
	q *db.Queries
}

func New(d *DB) *Store {
	return &Store{w: d.W, q: db.New(d.W)}
}

func (s *Store) LatestPlayedAt(ctx context.Context) (time.Time, error) {
	ms, err := s.q.LatestPlayedAt(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("latest played at: %w", err)
	}
	if ms == 0 {
		return time.Time{}, nil
	}

	// Convert the raw integer into a Go time.Time object
	return time.UnixMilli(ms), nil
}

// InsertPlays saves in the database a list of plays, in one transaction and returns how many plays were new
func (s *Store) InsertPlays(ctx context.Context, items []spotify.PlayHistory) (int64, error) {
	if len(items) == 0 {
		return 0, nil
	}

	tx, err := s.w.BeginTx(ctx, nil) // Start a transaction
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Tell the auto-generated sqlc engine to use this transaction
	q := s.q.WithTx(tx)

	var inserted int64
	for _, it := range items {
		// Local files have an empty id, fall back to the URI like the poller does
		trackID := it.Track.ID
		if trackID == "" {
			trackID = it.Track.URI
		}
		if trackID == "" {
			continue
		}

		// A zero PlayedAt would be stored as a huge negative timestamp
		if it.PlayedAt.UnixMilli() <= 0 {
			continue
		}

		// Order matters because of the foreign keys: track and artists before the links and the play
		err := q.UpsertTrack(ctx, db.UpsertTrackParams{
			ID:         trackID,
			Name:       it.Track.Name,
			AlbumName:  it.Track.Album.Name,
			DurationMs: int64(it.Track.DurationMs),
		})
		if err != nil {
			return 0, fmt.Errorf("upsert track %s: %w", trackID, err)
		}

		// Delete old artist links for this track 
		// The links must mirror the latest artist list, so drop stale ones first
		if err := q.DeleteTrackArtists(ctx, trackID); err != nil {
			return 0, fmt.Errorf("delete track artists %s: %w", trackID, err)
		}

		for i, a := range it.Track.Artists {
			if a.ID == "" {
				continue
			}

			// Save the artist to the database
			if err := q.UpsertArtist(ctx, db.UpsertArtistParams{ID: a.ID, Name: a.Name}); err != nil {
				return 0, fmt.Errorf("upsert artist %s: %w", a.ID, err)
			}

			// Link the artist to the track (requires both to already exist in the database)
			err := q.UpsertTrackArtist(ctx, db.UpsertTrackArtistParams{
				TrackID:  trackID,
				ArtistID: a.ID,
				Position: int64(i),
			})
			if err != nil {
				return 0, fmt.Errorf("upsert track artist %s/%s: %w", trackID, a.ID, err)
			}
		}

		n, err := q.InsertPlay(ctx, db.InsertPlayParams{
			PlayedAtMs: it.PlayedAt.UnixMilli(),
			TrackID:    trackID,
		})
		if err != nil {
			return 0, fmt.Errorf("insert play %d: %w", it.PlayedAt.UnixMilli(), err)
		}
		inserted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return inserted, nil
}
