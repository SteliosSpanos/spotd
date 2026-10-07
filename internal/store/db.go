package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

func dsn(path string, readOnly bool) string {
	q := url.Values{}

	// If two threads try to lock the database, wait up to 5 secs before throwing a SQLITE_BUSY error
	q.Add("_pragma", "busy_timeout(5000)")

	// Instead of locking the whole file, writes go to a temporary -wal file
	// This allows readers to read at the exact same time as a writer is writing
	q.Add("_pragma", "journal_mode(WAL)")

	// When combined with WAL it speeds up inserts
	q.Add("_pragma", "synchronous(NORMAL)")

	// Forces SQLite to respect relational rules
	q.Add("_pragma", "foreign_keys(1)")

	if readOnly {
		// Guarantees this connection cannot mutate data
		q.Add("mode", "ro")
	} else {
		// For the writer, grab the database lock the exact millisecond a transaction begins
		q.Set("_txlock", "immediate")
	}

	// SQLite parses this as a URI, so ?, # and % in the path must be percent-escaped or the file name gets truncated or rewritten
	u := url.URL{Path: path}
	return "file:" + u.EscapedPath() + "?" + q.Encode()
}

type DB struct {
	W *sql.DB // single writer connection
	R *sql.DB // pool for readers
}

func Open(ctx context.Context, path string) (_ *DB, err error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)

	// A failed open must not leak the pools
	defer func() {
		if err != nil {
			w.Close()
			r.Close()
		}
	}()

	// PingContext forces a real connection to ensure the file exists and is readable
	if err := w.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	return &DB{W: w, R: r}, nil
}

func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}
