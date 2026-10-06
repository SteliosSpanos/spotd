package spotify

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), slog.New(slog.DiscardHandler))
	c.baseURL = srv.URL

	return c
}

func TestCurrentlyPlaying(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/player/currently-playing" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"is_playing":true,"progress_ms":1234,"item":{"id":"abc","name":"Song","duration_ms":200000}}`)
	})

	p, err := c.CurrentlyPlaying(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsPlaying || p.ProgressMs != 1234 || p.Item == nil || p.Item.ID != "abc" || p.Item.DurationMs != 200000 {
		t.Errorf("got %+v item %+v", p, p.Item)
	}
}

func TestCurrentlyPlaying_NothingPlaying(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if _, err := c.CurrentlyPlaying(t.Context()); !errors.Is(err, ErrNoContent) {
		t.Fatalf("err = %v, want ErrNoContent", err)
	}
}

func TestRecentlyPlayed(t *testing.T) {
	var gotQuery string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"items":[{"track":{"id":"abc"},"played_at":"2026-10-01T10:00:00.123Z"}],"next":null}`)
	})

	items, err := c.RecentlyPlayed(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "limit=50" {
		t.Errorf("query without cursor = %q, want limit=50", gotQuery)
	}
	want := time.Date(2026, 10, 1, 10, 0, 0, 123_000_000, time.UTC)
	if len(items) != 1 || items[0].Track.ID != "abc" || !items[0].PlayedAt.Equal(want) {
		t.Errorf("items = %+v", items)
	}

	if _, err := c.RecentlyPlayed(t.Context(), time.UnixMilli(1_700_000_000_123)); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "limit=50&after=1700000000123" {
		t.Errorf("query with cursor = %q", gotQuery)
	}
}
