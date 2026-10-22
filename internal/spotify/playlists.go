package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Playlist contents live under /playlists/{id}/items and each entry holds the track in "item"

type Playlist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// SnapshotID acts like a version number for the playlist
	// Every time the playlist changes, Spotify generates a new SnapshotID
	SnapshotID string `json:"snapshot_id"`
	// Owner and collaborative are needed to filter ListPlaylists
	Collaborative bool `json:"collaborative"`
	Owner         struct {
		ID string `json:"id"`
	} `json:"owner"`
	Items struct {
		Total int `json:"total"`
	} `json:"items"`
}

type PlaylistItem struct {
	AddedAt time.Time `json:"added_at"`
	IsLocal bool      `json:"is_local"` // Local files can't be added back through the API
	Item    *Track    `json:"item"`     // nil when the track is no longer available
}

// Limits how many tracks we can add/remove in a single API call
const MaxItemsPerRequest = 100

// snapshot is the response body of every playlist mutation
type snapshot struct {
	SnapshotID string `json:"snapshot_id"`
}

// Only owned or collaborative playlists, followed ones can't be used by jobs
func (c *Client) MyPlaylists(ctx context.Context) ([]Playlist, error) {
	me, err := c.userID(ctx)
	if err != nil {
		return nil, err
	}

	var all []Playlist
	err = paginate(ctx, c, c.baseURL+"/me/playlists?limit=50", func(items []Playlist) error {
		for _, p := range items {
			if p.Owner.ID == me || p.Collaborative {
				all = append(all, p)
			}
		}
		return nil
	})

	return all, err
}

// userID fetches GET /me once and caches the id, a failed fetch is not cached
func (c *Client) userID(ctx context.Context) (string, error) {
	c.meMu.Lock()
	defer c.meMu.Unlock()

	if c.me != "" {
		return c.me, nil
	}

	var u struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/me", nil, &u, true); err != nil {
		return "", err
	}
	if u.ID == "" {
		return "", errors.New("spotify returned an empty user id")
	}

	c.me = u.ID
	return c.me, nil
}

// Playlist fetches basic metadata for a single playlist
func (c *Client) Playlist(ctx context.Context, id string) (*Playlist, error) {
	var p Playlist
	path := "/playlists/" + url.PathEscape(id) + "?fields=id,name,snapshot_id"

	if err := c.do(ctx, http.MethodGet, path, nil, &p, true); err != nil {
		return nil, err
	}

	return &p, nil
}

// PlaylistItems fetches every single track inside a specific playlist
func (c *Client) PlaylistItems(ctx context.Context, id string) ([]PlaylistItem, error) {
	var all []PlaylistItem
	first := c.baseURL + "/playlists/" + url.PathEscape(id) + "/items?limit=50"

	err := paginate(ctx, c, first, func(items []PlaylistItem) error {
		all = append(all, items...)
		return nil
	})

	return all, err
}

func (c *Client) CreatePlaylist(ctx context.Context, name, description string) (*Playlist, error) {
	// Instead of creating a dedicated struct just for requests on creating playlists, we use map[string]any
	// It automatically converts to JSON
	body := map[string]any{"name": name, "description": description, "public": false}

	var p Playlist
	if err := c.do(ctx, http.MethodPost, "/me/playlists", body, &p, false); err != nil {
		return nil, err
	}

	return &p, nil
}

// AddItems inserts tracks (uris) into a playlist
// If the position is negative, Spotify appends them to the very bottom
func (c *Client) AddItems(ctx context.Context, id string, uris []string, position int) (string, error) {
	body := map[string]any{"uris": uris}
	if position >= 0 {
		body["position"] = position
	}

	var s snapshot
	if err := c.do(ctx, http.MethodPost, "/playlists/"+url.PathEscape(id)+"/items", body, &s, false); err != nil {
		return "", err
	}

	return s.SnapshotID, nil
}

func (c *Client) RemoveItems(ctx context.Context, id string, uris []string, snapshotID string) (string, error) {
	// Spotify requires a JSON array format for deletion: {"items": [{"uri": "..."}]}
	items := make([]map[string]string, 0, len(uris))
	for _, u := range uris {
		items = append(items, map[string]string{"uri": u})
	}
	body := map[string]any{"items": items}

	if snapshotID != "" {
		body["snapshot_id"] = snapshotID
	}

	var s snapshot
	if err := c.do(ctx, http.MethodDelete, "/playlists/"+url.PathEscape(id)+"/items", body, &s, true); err != nil {
		return "", err
	}

	return s.SnapshotID, nil
}

// ReplaceItems wipes the entire playlist and replaces it with the provided tracks
func (c *Client) ReplaceItems(ctx context.Context, id string, uris []string) (string, error) {
	// A nil slice marshals to null; Spotify needs [] to clear the playlist
	if uris == nil {
		uris = []string{}
	}
	body := map[string]any{"uris": uris}

	var s snapshot
	if err := c.do(ctx, http.MethodPut, "/playlists/"+url.PathEscape(id)+"/items", body, &s, true); err != nil {
		return "", err
	}

	return s.SnapshotID, nil
}
