# 10 — Schema migrations with goose

## Concepts

- **Migrations** are ordered, versioned schema changes. The DB records which versions ran (goose uses a `goose_db_version` table).
- **Forward-only in practice**: write `Down` sections for local dev, but plan to fix forward in production.
- **Embedded migrations**: ship SQL files inside the binary with `embed.FS`, run them on daemon start. Users never run a migration tool manually.

## File format

`internal/store/migrations/00001_init.sql`:

```sql
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

-- +goose Down
DROP TABLE tracks;
DROP TABLE artists;
```

- Use sequential numbering (`goose create -s init sql` or `goose fix`) rather than timestamps for a solo project: deterministic order, readable.
- Each migration runs in a transaction by default. Use `-- +goose NO TRANSACTION` only for statements that cannot run in one.
- Statements containing semicolons (triggers) need `-- +goose StatementBegin` / `-- +goose StatementEnd`.

## Running embedded migrations with the Provider API

```go
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embedded embed.FS

func Migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	fsys, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	for _, r := range results {
		log.Info("applied migration", "version", r.Source.Version, "duration", r.Duration)
	}
	return nil
}
```

The Provider API (vs the older global `goose.SetDialect` / `goose.Up`) has no global state → safe in tests running in parallel. Check pkg.go.dev for current option names (`goose.WithVerbose`, etc.).

Call it with the **writer** connection before starting the poller or gRPC server.

## SQLite-specific migration notes

- SQLite's `ALTER TABLE` is limited (add/rename/drop column OK in modern versions, but changing types/constraints is not). The safe recipe for bigger changes:
  1. `CREATE TABLE new_x (...)`
  2. `INSERT INTO new_x SELECT ... FROM x`
  3. `DROP TABLE x`
  4. `ALTER TABLE new_x RENAME TO x`
  5. Recreate indexes
  — inside one migration, with `PRAGMA foreign_keys` considerations (foreign_keys can't be toggled inside a transaction; use `NO TRANSACTION` + explicit `BEGIN/COMMIT` if needed).
- Migrations are also the **schema input for sqlc** (it ignores `-- +goose Down` sections), so you have one source of truth.

## Testing

```go
func TestMigrationsUpDown(t *testing.T) {
	db := openTestDB(t)
	fsys, _ := fs.Sub(embedded, "migrations")
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil { t.Fatal(err) }
	ctx := context.Background()
	if _, err := p.Up(ctx); err != nil { t.Fatal(err) }
	if _, err := p.DownTo(ctx, 0); err != nil { t.Fatal(err) }
	if _, err := p.Up(ctx); err != nil { t.Fatal(err) } // re-up proves Down is correct
}
```

## Best practices

- Never edit a migration that has run on a real DB; add a new one.
- One logical change per migration.
- Back up before migrating on startup (`VACUUM INTO` the old DB if pending migrations exist) — cheap insurance.
- Log applied versions at startup.

## Pitfalls

- Forgetting `fs.Sub` → goose finds no files (paths include the `migrations/` prefix).
- Global goose functions in parallel tests → races on global dialect state.
- Mixing timestamp and sequential versions.

## Further reading

- https://pressly.github.io/goose/
- https://pkg.go.dev/github.com/pressly/goose/v3
- https://www.sqlite.org/lang_altertable.html
