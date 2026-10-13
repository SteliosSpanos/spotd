package store

import (
	"context"
	"fmt"
	"time"

	"github.com/SteliosSpanos/spotd/internal/store/db"
)

type ArtistCount struct {
	ID    string
	Name  string
	Plays int64
}

type TrackCount struct {
	ID      string
	Title   string
	Artists []string
	Album   string
	Plays   int64
}

type DayCount struct {
	Date       string
	Plays      int64
	ListenedMs int64
}

func sinceMs(since time.Time) int64 {
	if since.IsZero() {
		return 0
	}

	return since.UnixMilli()
}

func (s *Store) TopArtists(ctx context.Context, since time.Time, limit int) ([]ArtistCount, error) {
	rows, err := s.r.TopArtists(ctx, db.TopArtistsParams{FromMs: sinceMs(since), Lim: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("top artists: %w", err)
	}

	out := make([]ArtistCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, ArtistCount{ID: r.ID, Name: r.Name, Plays: r.Plays})
	}

	return out, nil
}

func (s *Store) TopTracks(ctx context.Context, since time.Time, limit int) ([]TrackCount, error) {
	rows, err := s.r.TopTracks(ctx, db.TopTracksParams{FromMs: sinceMs(since), Lim: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("top tracks: %w", err)
	}

	out := make([]TrackCount, 0, len(rows))
	for _, r := range rows {
		artists, err := s.r.TrackArtistsNames(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("artists of track %s: %w", r.ID, err)
		}

		out = append(out, TrackCount{ID: r.ID, Title: r.Name, Artists: artists, Album: r.AlbumName, Plays: r.Plays})
	}

	return out, nil
}

func (s *Store) ListeningByDay(ctx context.Context, since time.Time) ([]DayCount, error) {
	rows, err := s.r.ListeningByDay(ctx, sinceMs(since))
	if err != nil {
		return nil, fmt.Errorf("listening by day: %w", err)
	}

	out := make([]DayCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, DayCount{Date: r.Day, Plays: r.Plays, ListenedMs: r.ListenedMs})
	}

	return out, nil
}
