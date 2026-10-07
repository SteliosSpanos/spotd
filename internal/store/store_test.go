package store

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SteliosSpanos/spotd/internal/poller"
	"github.com/SteliosSpanos/spotd/internal/spotify"
	"github.com/pressly/goose/v3"
)

var _ poller.PlayStore = (*Store)(nil)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	d, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := Migrate(ctx, d.W, discard); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func play(id string, at time.Time, artists ...spotify.Artist) spotify.PlayHistory {
	return spotify.PlayHistory{
		Track: spotify.Track{
			ID:         id,
			URI:        "spotify:track:" + id,
			Name:       "name " + id,
			DurationMs: 1000,
			Artists:    artists,
			Album:      spotify.Album{Name: "album"},
		},
		PlayedAt: at,
	}
}

func count(t *testing.T, d *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := d.R.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestLatestPlayedAtEmpty(t *testing.T) {
	s := New(newDB(t))

	got, err := s.LatestPlayedAt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("want zero time, got %v", got)
	}
}

func TestInsertPlaysIdempotentAndLatest(t *testing.T) {
	ctx := context.Background()
	s := New(newDB(t))

	base := time.Date(2025, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	items := []spotify.PlayHistory{
		play("a", base, spotify.Artist{ID: "ar1", Name: "One"}),
		play("b", base.Add(time.Minute), spotify.Artist{ID: "ar1", Name: "One"}),
		play("a", base.Add(2*time.Minute), spotify.Artist{ID: "ar1", Name: "One"}),
	}

	n, err := s.InsertPlays(ctx, items)
	if err != nil || n != 3 {
		t.Fatalf("first insert: n=%d err=%v", n, err)
	}
	n, err = s.InsertPlays(ctx, items)
	if err != nil || n != 0 {
		t.Fatalf("second insert: n=%d err=%v", n, err)
	}

	got, err := s.LatestPlayedAt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := items[2].PlayedAt; got.UnixMilli() != want.UnixMilli() {
		t.Fatalf("latest = %v, want %v", got, want)
	}
}

func TestInsertPlaysEmpty(t *testing.T) {
	s := New(newDB(t))

	n, err := s.InsertPlays(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestInsertPlaysMultiArtist(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	s := New(d)

	it := play("t1", time.UnixMilli(1000),
		spotify.Artist{ID: "p", Name: "Primary"},
		spotify.Artist{ID: "", Name: "No id"},
		spotify.Artist{ID: "f", Name: "Featured"},
	)
	if _, err := s.InsertPlays(ctx, []spotify.PlayHistory{it}); err != nil {
		t.Fatal(err)
	}

	rows, err := d.R.Query(`SELECT artist_id, position FROM track_artists WHERE track_id = 't1' ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	want := []struct {
		id  string
		pos int
	}{{"p", 0}, {"f", 2}}
	i := 0
	for rows.Next() {
		var id string
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) || id != want[i].id || pos != want[i].pos {
			t.Fatalf("row %d = (%s,%d), want %+v", i, id, pos, want)
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("got %d rows, want %d", i, len(want))
	}
	if n := count(t, d, `SELECT COUNT(*) FROM artists`); n != 2 {
		t.Fatalf("artists = %d, want 2", n)
	}
}

func TestInsertPlaysLocalFile(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	s := New(d)

	it := play("x", time.UnixMilli(5000))
	it.Track.ID = ""
	it.Track.URI = "spotify:local:a:b:c:1"

	skipped := play("", time.UnixMilli(6000))
	skipped.Track.URI = ""

	n, err := s.InsertPlays(ctx, []spotify.PlayHistory{it, skipped})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM tracks WHERE id = ?`, it.Track.URI); c != 1 {
		t.Fatalf("local track rows = %d, want 1", c)
	}
}

func TestInsertPlaysRollsBack(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	s := New(d)

	_, err := d.W.Exec(`CREATE TRIGGER fail_play BEFORE INSERT ON plays
		WHEN NEW.track_id = 'bad' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if err != nil {
		t.Fatal(err)
	}

	items := []spotify.PlayHistory{
		play("good", time.UnixMilli(1000), spotify.Artist{ID: "a", Name: "A"}),
		play("bad", time.UnixMilli(2000)),
	}
	if n, err := s.InsertPlays(ctx, items); err == nil || n != 0 {
		t.Fatalf("want error and 0, got n=%d err=%v", n, err)
	}

	for _, tbl := range []string{"plays", "tracks", "artists", "track_artists"} {
		if c := count(t, d, `SELECT COUNT(*) FROM `+tbl); c != 0 {
			t.Fatalf("%s has %d rows after rollback", tbl, c)
		}
	}
}

func TestMigrateUpDownUp(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)

	p, err := goose.NewProvider(goose.DialectSQLite3, d.W, mustSub(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'plays'`); c != 0 {
		t.Fatal("plays table still exists after down")
	}
	if err := Migrate(ctx, d.W, discard); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'plays'`); c != 1 {
		t.Fatal("plays table missing after second up")
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	d := newDB(t)

	_, err := d.R.Exec(`INSERT INTO artists (id, name) VALUES ('x', 'y')`)
	if err == nil {
		t.Fatal("write through reader succeeded, mode=ro is not honoured")
	}
}

func mustSub(t *testing.T) fs.FS {
	t.Helper()
	fsys, err := fs.Sub(embedded, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func TestOpenSpecialCharsInPath(t *testing.T) {
	ctx := context.Background()

	for _, name := range []string{"a?b.db", "a#b.db", "a%41b.db", "a b.db", "a&b.db", "a=b.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			d, err := Open(ctx, filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			if err := Migrate(ctx, d.W, discard); err != nil {
				t.Fatal(err)
			}

			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("file with exact name missing: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if n := e.Name(); n != name && n != name+"-wal" && n != name+"-shm" {
					t.Fatalf("unexpected sibling %q", n)
				}
			}
		})
	}
}

func TestInsertPlaysReplacesTrackArtists(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	s := New(d)

	a := spotify.Artist{ID: "a", Name: "A"}
	b := spotify.Artist{ID: "b", Name: "B"}

	if _, err := s.InsertPlays(ctx, []spotify.PlayHistory{play("t", time.UnixMilli(1000), a, b)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertPlays(ctx, []spotify.PlayHistory{play("t", time.UnixMilli(2000), b)}); err != nil {
		t.Fatal(err)
	}

	if c := count(t, d, `SELECT COUNT(*) FROM track_artists WHERE track_id = 't'`); c != 1 {
		t.Fatalf("links = %d, want 1", c)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM track_artists WHERE track_id = 't' AND artist_id = 'b' AND position = 0`); c != 1 {
		t.Fatal("want single link b at position 0")
	}
	if c := count(t, d, `SELECT COUNT(*) FROM plays WHERE track_id = 't'`); c != 2 {
		t.Fatalf("plays = %d, want 2", c)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM artists`); c != 2 {
		t.Fatalf("artists = %d, want 2", c)
	}
}

func TestInsertPlaysSkipsZeroPlayedAt(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	s := New(d)

	n, err := s.InsertPlays(ctx, []spotify.PlayHistory{play("z", time.Time{})})
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c := count(t, d, `SELECT COUNT(*) FROM plays`); c != 0 {
		t.Fatalf("plays = %d, want 0", c)
	}
}
