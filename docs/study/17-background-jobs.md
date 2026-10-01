# 17 — Background jobs with progress (playlist dedupe, sort, backup, restore)

## Where jobs run

The TUI knows only the gRPC API and the daemon is the only process talking to Spotify, so **jobs run in the daemon**. The TUI starts a job, watches its progress over a server stream, and can cancel it. Bonus: jobs survive the TUI being closed.

```
TUI ── StartJob(type, playlist) ──► daemon: JobManager.Start → job id
TUI ── WatchJob(id) ─────────────► stream of {state, done, total, message}
TUI ── CancelJob(id) ────────────► cancel job's context
```

## Job model

```go
type State int

const (
	Pending State = iota
	Running
	Succeeded
	Failed
	Canceled
)

type Progress struct {
	JobID   string
	State   State
	Done    int
	Total   int
	Message string
	Err     string
}

type Job struct {
	ID       string
	Kind     string // "dedupe" | "sort" | "backup" | "restore"
	Target   string // playlist id
	cancel   context.CancelFunc
	progress *broadcast.Broadcaster[Progress] // reuse topic 08
}
```

## Job manager

```go
type Manager struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	busy    map[string]string // playlist id → job id (one mutating job per playlist)
	wg      sync.WaitGroup
	baseCtx context.Context   // daemon lifetime ctx: shutdown cancels all jobs
	log     *slog.Logger
}

type Runner func(ctx context.Context, report func(done, total int, msg string)) error

func (m *Manager) Start(kind, playlistID string, run Runner) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.busy[playlistID]; ok {
		return "", fmt.Errorf("%w: job %s already running on this playlist", ErrBusy, id)
	}

	ctx, cancel := context.WithCancel(m.baseCtx)
	j := &Job{ID: newID(), Kind: kind, Target: playlistID, cancel: cancel,
		progress: broadcast.New[Progress](8, 1000)}
	m.jobs[j.ID] = j
	m.busy[playlistID] = j.ID

	m.wg.Go(func() {
		defer cancel()
		defer m.finish(j)
		j.progress.Publish(Progress{JobID: j.ID, State: Running})
		err := run(ctx, func(done, total int, msg string) {
			j.progress.Publish(Progress{JobID: j.ID, State: Running, Done: done, Total: total, Message: msg})
		})
		final := Progress{JobID: j.ID, State: Succeeded}
		switch {
		case errors.Is(err, context.Canceled):
			final.State = Canceled
		case err != nil:
			final.State, final.Err = Failed, err.Error()
			m.log.Error("job failed", "job", j.ID, "kind", kind, "err", err)
		}
		j.progress.Publish(final)
	})
	return j.ID, nil
}

func (m *Manager) finish(j *Job) {
	m.mu.Lock()
	delete(m.busy, j.Target)
	m.mu.Unlock()
	j.progress.Close() // ends WatchJob streams after the final state was delivered from the snapshot/buffer
}

// Wait is called during shutdown after baseCtx is cancelled.
func (m *Manager) Wait() { m.wg.Wait() }
```

Using the broadcaster's "last value snapshot" means a TUI that connects mid-job immediately sees current progress. Keep finished jobs in the map for a while (or persist them in SQLite) so `WatchJob` on a finished job returns its final state.

## The jobs themselves

**Backup** (safe, read-only on Spotify):
1. Page through `GET /playlists/{id}/items` (renamed from `/tracks` in Feb 2026).
2. Write JSON `{playlist metadata, snapshot_id, items:[{uri, added_at}]}` to `$XDG_DATA_HOME/spotd/backups/<id>-<timestamp>.json`.
3. Write to a temp file then `os.Rename` → atomic, never a half-written backup. Permissions `0600`.

**Restore**:
1. Read backup, `POST /me/playlists` to create a *new* playlist (safer than overwriting).
2. `POST /playlists/{id}/items` in chunks of ≤100 URIs, reporting progress per chunk.

**Dedupe**:
1. Fetch all items; duplicates = same track URI (optionally same normalized `name + primary artist` for different releases — make it an option, it's lossy).
2. **Automatically back up first.**
3. Remove duplicates with `DELETE /playlists/{id}/items`, passing `snapshot_id` so concurrent edits are detected. Chunk ≤100.

**Sort** (by artist, album, date added, …):
- Option A: compute the target order and issue `PUT /playlists/{id}/items` reorder operations (`range_start`, `insert_before`) — preserves `added_at`, many calls.
- Option B: replace all items with the sorted list — few calls, but **loses `added_at`** and is destructive. Back up first. Document the trade-off.

Verify exact request-body field names in the current reference — the Feb-2026 rename changed endpoint paths and response fields.

## Cancellation & resilience

- Every Spotify call uses the job `ctx`; check `ctx.Err()` between chunks.
- Retries come from the Spotify client (04). Long jobs will hit 429 — the shared cooldown matters here.
- Cap job concurrency against the API: one mutating job per playlist, and a global semaphore (e.g. 2 running jobs).
- On daemon shutdown: cancel `baseCtx`, `Wait()` with a timeout. A cancelled dedupe may have removed some duplicates — fine because dedupe is idempotent (running it again finishes the work). Make each job idempotent or resumable and say so.

## gRPC

```go
func (s *Playlists) WatchJob(req *pb.WatchJobRequest, stream grpc.ServerStreamingServer[pb.WatchJobResponse]) error {
	ch, cancel, err := s.jobs.Subscribe(req.GetJobId())
	if err != nil {
		return status.Error(codes.NotFound, "unknown job")
	}
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case p, ok := <-ch:
			if !ok {
				return nil // job finished; final state already sent
			}
			if err := stream.Send(toProto(p)); err != nil {
				return err
			}
		}
	}
}
```

Note: for job progress, *don't* disconnect laggards as aggressively as live events — or ensure the final state is never dropped (e.g. `WatchJob` re-reads the job's final state after the channel closes and sends it).

## Testing

- Fake Spotify with N playlist items including duplicates; run dedupe; assert removal calls and progress sequence (`0/N … N/N`, then `Succeeded`).
- Cancel mid-job → state `Canceled`, goroutine exits (`goleak`).
- Two jobs on the same playlist → second returns `ErrBusy` → gRPC `FailedPrecondition`.
- Backup writes atomically (no partial file if writer fails).

## Pitfalls

- Running jobs in the TUI process (violates "TUI knows only gRPC", dies when you close the TUI).
- No backup before destructive operations.
- Ignoring `snapshot_id` → clobbering edits made in the Spotify app meanwhile.
- Unbounded job map growth — evict finished jobs after a TTL.
