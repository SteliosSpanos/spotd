# 05 — Spotify Web API (what spotd uses)

> **Important (2026):** Spotify changed Development Mode in February 2026
> (enforced for new apps 2026-02-11, existing dev-mode apps 2026-03-09).
> Many older tutorials and libraries are now wrong. Summary below; verify in the
> [changelog](https://developer.spotify.com/documentation/web-api/references/changes/february-2026)
> and [migration guide](https://developer.spotify.com/documentation/web-api/tutorials/february-2026-migration-guide).

## Development Mode constraints

- The app **owner must have Spotify Premium**.
- New dev-mode apps: max **5 users** (allow-listed in the dashboard). Fine for a personal tool.
- Player control endpoints (play/pause/skip/queue) require the *listening* user to have Premium.

## Endpoints spotd needs

All under `https://api.spotify.com/v1`. Unaffected by the 2026 changes unless noted.

### Player (fast loop + commands)

| Purpose | Method & path | Notes |
|---------|---------------|-------|
| Currently playing | `GET /me/player/currently-playing` | **204** when nothing is playing. `item` can be a track or episode (`currently_playing_type`); use `additional_types=track,episode` |
| Full playback state | `GET /me/player` | includes device, shuffle, repeat |
| Play / resume | `PUT /me/player/play` | optional body `{context_uri, uris, offset, position_ms}` |
| Pause | `PUT /me/player/pause` | |
| Next / previous | `POST /me/player/next`, `/previous` | **not idempotent** |
| Add to queue | `POST /me/player/queue?uri=spotify:track:…` | not idempotent |
| Devices | `GET /me/player/devices` | needed when you get `404 NO_ACTIVE_DEVICE` |

### History (slow loop)

`GET /me/player/recently-played?limit=50&after=<unix ms>`

- Max `limit` 50. Cursor-based: `after` **or** `before` (unix milliseconds), not both.
- Response: `items[] { track, played_at (ISO-8601 UTC with ms), context }`, plus `cursors.after/before`.
- Only covers recent history — Spotify does not give you unlimited backfill. This is why the slow loop must run regularly: what you miss is gone.
- Polling every ~3 min with `limit=50` cannot miss tracks unless you play >50 tracks in 3 minutes.

### Stats

`GET /me/top/{artists|tracks}?time_range=short_term|medium_term|long_term&limit=50` — Spotify's own computation. Your own stats (listening by day, top artists from *your* plays table) come from SQLite — that's the more impressive part.

### Playlists (renamed in 2026)

| Purpose | **Current** endpoint | Removed (old) |
|---------|----------------------|---------------|
| My playlists | `GET /me/playlists` | `GET /users/{id}/playlists` |
| Create playlist | `POST /me/playlists` | `POST /users/{id}/playlists` |
| Get items | `GET /playlists/{id}/items` | `GET /playlists/{id}/tracks` |
| Add items | `POST /playlists/{id}/items` | `POST …/tracks` |
| Remove items | `DELETE /playlists/{id}/items` | `DELETE …/tracks` |
| Reorder / replace | `PUT /playlists/{id}/items` | `PUT …/tracks` |
| Details | `GET/PUT /playlists/{id}` | |

Response shape also changed: `tracks.items[].track` → `items.items[].item`. Full item lists are only returned for the user's own playlists; other playlists return metadata only. Check the reference for exact request-body field names before implementing.

Other 2026 removals that matter: batch lookups (`GET /tracks?ids=…`, `GET /artists?ids=…`) are gone; `popularity`, `external_ids`, `available_markets`, artist `followers` fields removed. Don't design features around them.

## Pagination

Offset-based endpoints return `{items, limit, offset, total, next}`. Follow `next` until null:

```go
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
		if err := fn(p.Items); err != nil {
			return err
		}
		url = p.Next
	}
	return nil
}
```

Guard `getAbs` so it only follows URLs on `https://api.spotify.com/` (don't send your bearer token to a host you didn't choose).

## Playlist snapshots

Mutating playlist endpoints return a `snapshot_id`. Pass it on removals/reorders so concurrent edits are detected rather than silently clobbered. Batch mutations in chunks of ≤100 items.

## DTO tips

```go
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
	PlayedAt time.Time `json:"played_at"` // RFC 3339 parses directly
}
```

- Decode only fields you use. Unknown fields are ignored by `encoding/json`, which keeps you resilient to API changes.
- `id` may be null for local files — handle empty IDs.
- Keep DTOs in `internal/spotify`; convert to domain/DB types at the boundary.

## Best practices

- Treat 204 from currently-playing as "stopped", not an error.
- Map `404 NO_ACTIVE_DEVICE` to a friendly gRPC status (`FailedPrecondition`, "open Spotify on a device first").
- Use `market=from_token` where relevant.
- Record fixtures of real responses (with tokens stripped) under `testdata/` for decoder tests.

## Further reading

- https://developer.spotify.com/documentation/web-api
- https://developer.spotify.com/documentation/web-api/concepts/scopes
- https://developer.spotify.com/documentation/web-api/concepts/rate-limits
