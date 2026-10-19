package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/SteliosSpanos/spotd/internal/auth"
	"github.com/SteliosSpanos/spotd/internal/events"
	"github.com/SteliosSpanos/spotd/internal/jobs"
	"github.com/SteliosSpanos/spotd/internal/poller"
	"github.com/SteliosSpanos/spotd/internal/server"
	"github.com/SteliosSpanos/spotd/internal/spotify"
	"github.com/SteliosSpanos/spotd/internal/store"
)

const shutdownTimeout = 10 * time.Second

func runDaemon(ctx context.Context, log *slog.Logger, clientID, socketPath, dbPath string) error {
	// Load the refresh token first, so a logged-out user fails before anything gets created
	secrets := auth.KeyringStore{}
	rt, err := secrets.RefreshToken()
	if err != nil {
		return err
	}

	// Ensure the XDG Data folder actually exists on the hard drive
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := store.Migrate(ctx, db.W, log); err != nil {
		return err
	}

	tctx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})

	// The auto-refreshing OAuth engine
	ts := auth.NewTokenSource(tctx, auth.Config(clientID), rt, secrets.SaveRefreshToken, log)
	api := spotify.New(ts, log)
	// only replay playback state to new watchers, never a stale HistoryUpdated
	bus := events.NewWithReplay(16, events.Event.Replayable)
	st := store.New(db)
	p := poller.New(api, st, bus, log)

	ln, err := server.ListenUnix(socketPath)
	if err != nil {
		return err
	}

	// errgroup manages multiple background threads
	// If one of the crashes, it automatically cancels the context for all of them
	g, gctx := errgroup.WithContext(ctx)

	// Jobs live al long as gctx
	jm := jobs.NewManager(gctx, log)
	runners := jobs.NewPlaylists(api, filepath.Join(filepath.Dir(dbPath), "backups"))

	srv := server.New(
		log,
		server.NewPlayer(api, bus),
		server.NewStats(st),
		server.NewPlaylists(api, runners, jm),
	)

	g.Go(func() error { return p.RunFast(gctx) })

	g.Go(func() error { return p.RunSlow(gctx) })

	g.Go(func() error {
		log.Info("grpc listening", "socket", socketPath)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}

		return nil
	})

	g.Go(func() error {
		// Wait here forever until Ctrl+C is pressed (or another thread crashes)
		<-gctx.Done()
		log.Info("shutting down", "cause", context.Cause(gctx))

		bus.Close()

		// Give the gRPC server 10 secs to finish currently active requests cleanly
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()

		select {
		case <-done: // Shut down cleanly, because the 'done' channel was closed
		case <-time.After(shutdownTimeout): // 10 secs passed, force kill
			log.Warn("graceful stop timed out, forcing")
			srv.Stop()
		}

		// gctx is already cancelled, so the jobs are stopping, wait for them to return
		jm.Wait()

		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("remove socket", "err", err)
		}

		return nil
	})

	err = g.Wait()

	// Cancellation from a signal is a clean shutdown, not a failure
	if errors.Is(err, context.Canceled) {
		err = nil
	}

	log.Info("daemon stopped", "err", err)
	return err
}
