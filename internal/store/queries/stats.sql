-- name: TopArtists :many
-- Every artist credited on a track gets the play, not only the primary one
SELECT a.id, a.name, COUNT(*) AS plays
FROM plays p 
JOIN track_artists ta ON ta.track_id = p.track_id
JOIN artists a ON a.id = ta.artist_id
WHERE p.played_at_ms >= sqlc.arg(from_ms)
GROUP BY a.id 
ORDER BY plays DESC, a.name 
LIMIT sqlc.arg(lim);

-- name: TopTracks :many 
SELECT t.id, t.name, t.album_name, COUNT(*) AS plays 
FROM plays p 
JOIN tracks t ON t.id = p.track_id 
WHERE p.played_at_ms >= sqlc.arg(from_ms)
GROUP BY t.id 
ORDER BY plays DESC, t.name 
LIMIT sqlc.arg(lim);

-- name: TrackArtistsNames :many 
SELECT a.name 
FROM track_artists ta 
JOIN artists a ON a.id = ta.artist_id
WHERE ta.track_id = ?
ORDER BY ta.position;

-- name: ListeningByDay :many 
SELECT 
  CAST(date(p.played_at_ms / 1000, 'unixepoch', 'localtime') AS TEXT) AS day,
  COUNT(*) AS plays,
  CAST(COALESCE(SUM(t.duration_ms), 0) AS INTEGER) AS listened_ms
FROM plays p 
JOIN tracks t ON t.id = p.track_id
WHERE p.played_at_ms >= sqlc.arg(from_ms)
GROUP BY day 
ORDER BY day;
