-- name: UpsertArtist :exec
INSERT INTO artists (id, name)
VALUES (?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name;

-- name: UpsertTrack :exec
INSERT INTO tracks (id, name, album_name, duration_ms)
VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name,
    album_name = excluded.album_name,
    duration_ms = excluded.duration_ms;

-- name: UpsertTrackArtist :exec
INSERT INTO track_artists (track_id, artist_id, position)
VALUES (?, ?, ?)
ON CONFLICT (track_id, artist_id) DO UPDATE SET
    position = excluded.position;

-- name: DeleteTrackArtists :exec
DELETE FROM track_artists WHERE track_id = ?;

-- name: InsertPlay :execrows
INSERT INTO plays (played_at_ms, track_id, context_uri)
VALUES (?, ?, ?)
ON CONFLICT (played_at_ms) DO NOTHING;

-- name: LatestPlayedAt :one
SELECT CAST(COALESCE(MAX(played_at_ms), 0) AS INTEGER) AS played_at_ms FROM plays;
