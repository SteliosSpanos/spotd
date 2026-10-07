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

// Migrate applies every pending migration
// Call it with the writer connection before starting the poller or the gRPC server
func Migrate(ctx context.Context, db *sql.DB, l *slog.Logger) error {
	// Strip the "migrations/" prefix, goose expects the files at the root of the filesystem
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
		l.Info("applied migration", "version", r.Source.Version, "duration", r.Duration)
	}

	return nil
}
