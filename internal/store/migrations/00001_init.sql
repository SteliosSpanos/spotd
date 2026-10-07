-- +goose Up
CREATE TABLE artists (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
) STRICT;

CREATE TABLE tracks (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    album_name  TEXT NOT NULL,
    duration_ms INTEGER NOT NULL
) STRICT;

-- Many-to-Many relationship
CREATE TABLE track_artists (
    track_id  TEXT NOT NULL REFERENCES tracks(id),
    artist_id TEXT NOT NULL REFERENCES artists(id),
    position  INTEGER NOT NULL, -- Who is the main artist (0 = primary, 1 = featured)
    PRIMARY KEY (track_id, artist_id)
) STRICT;

CREATE TABLE plays (
    played_at_ms INTEGER PRIMARY KEY,  -- The idempotency key
    track_id     TEXT NOT NULL REFERENCES tracks(id),
    context_uri  TEXT
) STRICT;

CREATE INDEX plays_track_idx ON plays(track_id);

-- +goose Down
DROP INDEX plays_track_idx;
DROP TABLE plays;
DROP TABLE track_artists;
DROP TABLE tracks;
DROP TABLE artists;

