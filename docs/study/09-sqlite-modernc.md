# 09 — SQLite with modernc.org/sqlite (pure Go, no CGO)

## Why this driver

- `modernc.org/sqlite` is SQLite transpiled to Go: **no CGO**, so cross-compiling with GoReleaser for linux/darwin/windows × amd64/arm64 just works.
- Trade-off vs `mattn/go-sqlite3` (CGO): somewhat slower, larger binary. Irrelevant at spotd's scale; mention the trade-off in your README.
- Driver name is `"sqlite"` (not `"sqlite3"`).

## Opening the database properly

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")   // wait up to 5s on locks instead of SQLITE_BUSY
	q.Add("_pragma", "journal_mode(WAL)")    // readers don't block the writer
	q.Add("_pragma", "synchronous(NORMAL)")  // safe with WAL, much faster than FULL
	q.Add("_pragma", "foreign_keys(1)")      // off by default in SQLite!
	q.Set("_txlock", "immediate")            // BEGIN IMMEDIATE: take write lock up front
	return "file:" + path + "?" + q.Encode()
}

type DB struct {
	W *sql.DB // single writer connection
	R *sql.DB // pool for readers (stats queries)
}

func Open(ctx context.Context, path string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1) // SQLite allows one writer at a time; serialize in Go, not via SQLITE_BUSY

	// Separate reader pool. Optionally add "&mode=ro" (SQLite URI param) to make
	// accidental writes through it fail — verify the driver honours it in a test.
	r, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)

	if err := w.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return &DB{W: w, R: r}, nil
}

func (d *DB) Close() error { return errors.Join(d.R.Close(), d.W.Close()) }
```

PRAGMAs are **per connection**. Putting them in the DSN (`_pragma=`) applies them to every connection the pool opens. Running `db.Exec("PRAGMA foreign_keys=ON")` once only affects one pooled connection — a classic bug.

## Why `_txlock=immediate`

Default `BEGIN` is DEFERRED: a transaction starts as a reader and tries to upgrade to writer on first write. Two such transactions can deadlock-ish and one gets `SQLITE_BUSY` immediately (busy_timeout does not help on upgrade). `BEGIN IMMEDIATE` takes the write lock at start, so `busy_timeout` works as expected.

## Schema for spotd

Normalised so stats like "top artists" are correct for multi-artist tracks:

```sql
CREATE TABLE artists (
    id   TEXT PRIMARY KEY,           -- spotify artist id
    name TEXT NOT NULL
);

CREATE TABLE tracks (
    id          TEXT PRIMARY KEY,    -- spotify track id
    name        TEXT NOT NULL,
    album_name  TEXT NOT NULL,
    duration_ms INTEGER NOT NULL
);

CREATE TABLE track_artists (
    track_id  TEXT NOT NULL REFERENCES tracks(id),
    artist_id TEXT NOT NULL REFERENCES artists(id),
    position  INTEGER NOT NULL,
    PRIMARY KEY (track_id, artist_id)
);

CREATE TABLE plays (
    played_at_ms INTEGER PRIMARY KEY,  -- unique: one play per instant; the idempotency key
    track_id     TEXT NOT NULL REFERENCES tracks(id),
    context_uri  TEXT
) STRICT;

CREATE INDEX plays_track_idx ON plays(track_id);
```

- `STRICT` tables (SQLite ≥ 3.37) enforce column types — use them.
- Times as **INTEGER unix ms UTC**: sortable, compact, no format ambiguity. Convert to local time only for display/grouping.
- `INTEGER PRIMARY KEY` on `played_at_ms` makes it the rowid → fast range scans by time.

Ingest in one transaction:

```go
tx, err := db.W.BeginTx(ctx, nil)
if err != nil { return 0, err }
defer tx.Rollback() // no-op after Commit

q := queries.WithTx(tx) // sqlc
for _, it := range items {
	// UPSERT track + artists (ON CONFLICT DO UPDATE SET name=excluded.name)
	// INSERT play ON CONFLICT DO NOTHING
}
return inserted, tx.Commit()
```

## Stats queries

Listening by day (local time):

```sql
-- name: ListeningByDay :many
SELECT date(p.played_at_ms / 1000, 'unixepoch', 'localtime') AS day,
       COUNT(*)                                             AS plays,
       SUM(t.duration_ms)                                   AS ms
FROM plays p JOIN tracks t ON t.id = p.track_id
WHERE p.played_at_ms >= ?
GROUP BY day
ORDER BY day;
```

Top artists:

```sql
-- name: TopArtists :many
SELECT a.id, a.name, COUNT(*) AS plays
FROM plays p
JOIN track_artists ta ON ta.track_id = p.track_id
JOIN artists a        ON a.id = ta.artist_id
WHERE p.played_at_ms BETWEEN ? AND ?
GROUP BY a.id
ORDER BY plays DESC
LIMIT ?;
```

Check plans with `EXPLAIN QUERY PLAN` and add indexes where you see `SCAN`.

## Backups

`VACUUM INTO '/path/backup.db'` produces a consistent copy while the daemon runs — a nice extra RPC.

## Testing

- Use a temp file DB per test: `filepath.Join(t.TempDir(), "test.db")` — realistic (WAL needs a file) and isolated.
- Run real migrations in tests (see 10). Don't mock the database for store tests.
- `:memory:` gives each *connection* its own DB — confusing with pools. Prefer temp files.

## Best practices

- One writer connection; route all writes through it.
- `context`-aware methods everywhere (`QueryContext`, `ExecContext`).
- Always `defer rows.Close()` and check `rows.Err()` (sqlc does this for you).
- File permissions: create the data dir `0700` (see 22).
- Close the DB last during shutdown, after all goroutines using it have stopped.

## Pitfalls

- `database is locked (SQLITE_BUSY)`: missing busy_timeout, deferred transactions, or a long-lived read transaction (unclosed `rows`).
- Foreign keys silently not enforced (not enabled per connection).
- Storing timestamps as strings with mixed formats.
- Using `mattn` examples with the `sqlite3` driver name.

## Further reading

- https://pkg.go.dev/modernc.org/sqlite
- https://www.sqlite.org/wal.html
- https://www.sqlite.org/lang_upsert.html
- https://www.sqlite.org/stricttables.html
- https://www.sqlite.org/lang_transaction.html
