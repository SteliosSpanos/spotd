package spotify

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Artist struct {
	ID   string `json:"id"`
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type Album struct {
	ID   string `json:"id"`
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type Track struct {
	ID         string   `json:"id"`
	URI        string   `json:"uri"`
	Name       string   `json:"name"`
	DurationMs int      `json:"duration_ms"`
	Artists    []Artist `json:"artists"`
	Album      Album    `json:"album"`
}

type PlayHistory struct {
	Track    Track     `json:"track"`
	PlayedAt time.Time `json:"played_at"`
}

// Playing is the response of GET /me/player/currently-playing.
// Item is nil when Spotify reports playback without a track or episode (e.g. an ad).
type Playing struct {
	IsPlaying  bool   `json:"is_playing"`
	ProgressMs int    `json:"progress_ms"`
	Item       *Track `json:"item"`
}

// CurrentlyPlaying returns ErrNoContent when nothing is playing.
func (c *Client) CurrentlyPlaying(ctx context.Context) (*Playing, error) {
	var p Playing
	if err := c.do(ctx, http.MethodGet, "/me/player/currently-playing?additional_types=track,episode", nil, &p, true); err != nil {
		return nil, err
	}

	return &p, nil
}

// RecentlyPlayed returns up to 50 plays that finished after the given time.
// A zero (or pre-epoch) time means no cursor: the most recent plays are returned.
func (c *Client) RecentlyPlayed(ctx context.Context, after time.Time) ([]PlayHistory, error) {
	path := "/me/player/recently-played?limit=50"
	if ms := after.UnixMilli(); !after.IsZero() && ms > 0 {
		path += "&after=" + strconv.FormatInt(ms, 10)
	}

	var p Page[PlayHistory]
	if err := c.do(ctx, http.MethodGet, path, nil, &p, true); err != nil {
		return nil, err
	}

	return p.Items, nil
}

// Spotify limits lists to 50 items per request
// If we have 200 items, Spotify sends the first 50 and provides a 'next' URL to get the next 50
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
	Total int    `json:"total"`
}

func paginate[T any](ctx context.Context, c *Client, first string, fn func([]T) error) error {
	url := first
	for url != "" {
		var p Page[T]
		if err := c.getAbs(ctx, url, &p); err != nil {
			return err
		}

		// Pass the items we just fetched into the callback function 'fn' provided by the user
		// For example, this function might save these 50 tracks to a SQLite database
		if err := fn(p.Items); err != nil {
			return err
		}

		url = p.Next
	}
	return nil
}

// getAbs GETs an absolute URL
// It refuses URLs outside c.baseURL so the bearer token is never sent to another host.
func (c *Client) getAbs(ctx context.Context, absURL string, out any) error {
	path, ok := strings.CutPrefix(absURL, c.baseURL+"/")
	if !ok {
		return fmt.Errorf("refusing to follow url outside %s: %q", c.baseURL, absURL)
	}

	return c.do(ctx, http.MethodGet, "/"+path, nil, out, true)
}
