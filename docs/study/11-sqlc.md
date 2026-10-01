# 11 — Type-safe SQL with sqlc

## Concepts

- You write **SQL**; sqlc parses your schema + queries and **generates Go** (structs + methods) at build time.
- No ORM, no reflection at runtime, compile-time checked column names and types.
- Generated code depends only on `database/sql`, so it works with `modernc.org/sqlite`.

## Config: `sqlc.yaml` (v2 format)

```yaml
version: "2"
sql:
  - engine: "sqlite"
    schema: "internal/store/migrations"   # reads goose files, ignores Down sections
    queries: "internal/store/queries"
    gen:
      go:
        package: "db"
        out: "internal/store/db"
        emit_interface: true          # generates a Querier interface → easy fakes
        emit_json_tags: false
        emit_empty_slices: true       # :many returns [] not nil
```

Run: `sqlc generate`. Verify in CI: `sqlc diff` (exits non-zero if generated code is stale) or `sqlc generate && git diff --exit-code`. `sqlc vet` runs lint rules on queries.

## Query annotations

```sql
-- internal/store/queries/plays.sql

-- name: UpsertTrack :exec
INSERT INTO tracks (id, name, album_name, duration_ms)
VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name,
    album_name = excluded.album_name,
    duration_ms = excluded.duration_ms;

-- name: InsertPlay :execrows
INSERT INTO plays (played_at_ms, track_id, context_uri)
VALUES (?, ?, ?)
ON CONFLICT (played_at_ms) DO NOTHING;

-- name: LatestPlayedAt :one
SELECT COALESCE(MAX(played_at_ms), 0) AS played_at_ms FROM plays;

-- name: TopTracks :many
SELECT t.id, t.name, COUNT(*) AS plays
FROM plays p JOIN tracks t ON t.id = p.track_id
WHERE p.played_at_ms BETWEEN sqlc.arg(from_ms) AND sqlc.arg(to_ms)
GROUP BY t.id
ORDER BY plays DESC
LIMIT sqlc.arg(lim);
```

| Annotation | Generated return |
|------------|------------------|
| `:one` | `(Row, error)` — `sql.ErrNoRows` if none |
| `:many` | `([]Row, error)` |
| `:exec` | `error` |
| `:execrows` | `(int64, error)` — affected rows |
| `:execresult` | `(sql.Result, error)` |

`sqlc.arg(name)` (or `@name`) gives parameters readable names in the generated params struct instead of `Column1`, `Column2`.

## Generated code usage

```go
q := db.New(conn)                         // conn: *sql.DB or *sql.Tx (DBTX interface)
n, err := q.InsertPlay(ctx, db.InsertPlayParams{
	PlayedAtMs: it.PlayedAt.UnixMilli(),
	TrackID:    it.Track.ID,
	ContextUri: sql.NullString{String: it.ContextURI, Valid: it.ContextURI != ""},
})
```

Transactions:

```go
func (s *Store) InsertPlays(ctx context.Context, items []spotify.PlayHistory) (int64, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	q := s.q.WithTx(tx)
	var inserted int64
	for _, it := range items {
		if err := q.UpsertTrack(ctx, toTrackParams(it.Track)); err != nil {
			return 0, fmt.Errorf("upsert track %s: %w", it.Track.ID, err)
		}
		n, err := q.InsertPlay(ctx, toPlayParams(it))
		if err != nil {
			return 0, err
		}
		inserted += n
	}
	return inserted, tx.Commit()
}
```

## Wrap, don't leak

Keep generated types inside `internal/store`. Expose a `Store` with domain-level methods (`InsertPlays`, `TopArtists(ctx, Range)`), and convert sqlc rows → domain types there. The gRPC layer should never import `internal/store/db`.

## Type mapping notes (SQLite)

- SQLite has dynamic typing; sqlc infers Go types from declared column types. `INTEGER` → `int64`, `TEXT` → `string`, nullable → `sql.NullString` / `sql.NullInt64`.
- Aggregates like `COUNT(*)` → `int64`; `SUM(...)` may become `sql.NullFloat64`/`interface{}` depending on version — wrap with `CAST(... AS INTEGER)` / `COALESCE` to pin the type.
- Use `overrides` in `sqlc.yaml` if you need custom types.

## Best practices

- Name every query; keep one `.sql` file per aggregate (plays, stats, playlists).
- Commit generated code + CI staleness check.
- Integration-test queries against a real migrated SQLite DB.

## Pitfalls

- Editing generated files by hand (overwritten next run).
- Ambiguous column names in joins → explicit aliases.
- Forgetting `emit_empty_slices` and serializing `nil` slices in APIs.

## Further reading

- https://docs.sqlc.dev/en/latest/tutorials/getting-started-sqlite.html
- https://docs.sqlc.dev/en/latest/reference/config.html
- https://docs.sqlc.dev/en/latest/reference/query-annotations.html
