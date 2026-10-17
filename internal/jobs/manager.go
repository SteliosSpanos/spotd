package jobs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/SteliosSpanos/spotd/internal/events"
)

// Jobs run inside the daemon, not the TUI
// The daemon is the only process that talks to Spotify, and a job keeps running even if the TUI that started it is closed

var (
	ErrBusy     = errors.New("another job is already running on this playlist")
	ErrNotFound = errors.New("unknown job")
)

const (
	maxRunning  = 2
	keepHistory = 10 * time.Minute // How long a finished job can still be watched
)

type State int

const (
	Pending State = iota // Waiting in line for a slot
	Running              // Actively talking to Spotify
	Succeeded
	Failed
	Canceled
)

type Kind int

const (
	Backup Kind = iota
	Restore
	Dedupe
	Sort
)

func (k Kind) String() string {
	switch k {
	case Backup:
		return "backup"
	case Restore:
		return "restore"
	case Dedupe:
		return "dedupe"
	case Sort:
		return "sort"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// Progress is a snapshot of a job, it's what WatchJob streams to the TUI
type Progress struct {
	JobID   string
	State   State
	Done    int
	Total   int
	Message string
	Err     string
}

// Report is how a running job tells the manager how far it got
type Report func(done, total int, msg string)

// Runner is the actual work, it must stop as soon as ctx is cancelled
type Runner func(ctx context.Context, report Report) error

type job struct {
	id       string
	kind     Kind
	target   string                        // playlist ID
	cancel   context.CancelFunc            // A button to kill this specific job
	progress *events.Broadcaster[Progress] // Every individual job broadcasts its own progress
	final    *Progress                     // Stores the final result so users can check it later
	ended    time.Time
}

type Manager struct {
	mu      sync.Mutex
	jobs    map[string]*job
	busy    map[string]string // Prevents running two different jobs on the same playlist
	sem     chan struct{}
	wg      sync.WaitGroup  // Keeps track of how many jobs are running so the daemon can shut them down safely
	baseCtx context.Context // The daemon's lifetime: shutting down cancels every job
	log     *slog.Logger
}

func NewManager(ctx context.Context, l *slog.Logger) *Manager {
	return &Manager{
		jobs:    make(map[string]*job),
		busy:    make(map[string]string),
		sem:     make(chan struct{}, maxRunning),
		baseCtx: ctx,
		log:     l,
	}
}

// Start queues a new job and starts executing it in the background
func (m *Manager) Start(kind Kind, playlistID string, run Runner) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clean up old jobs to save memory
	m.evictLocked()

	// Never allow two operations to mutate the same playlist at once
	if id, ok := m.busy[playlistID]; ok {
		return "", fmt.Errorf("%w (job %s)", ErrBusy, id)
	}

	// Create a specific kill switch for this job
	ctx, cancel := context.WithCancel(m.baseCtx)
	j := &job{
		id:       rand.Text()[:8],
		kind:     kind,
		target:   playlistID,
		cancel:   cancel,
		progress: events.New[Progress](16),
	}
	m.jobs[j.id] = j
	m.busy[playlistID] = j.id

	j.progress.Publish(Progress{JobID: j.id, State: Pending, Message: "waiting for a free slot"})

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.finish(j, m.run(ctx, j, run))
	}()

	// Return the id to the CLI so the user can track it
	return j.id, nil
}

// run handles the semaphore waiting room and executes the work
func (m *Manager) run(ctx context.Context, j *job, run Runner) error {
	select {
	// We try to push an empty struct into the sem channel
	// If the channel already has 2 items in it, this will freeze here until one finishes
	case m.sem <- struct{}{}:
		// We acquired the slot and proceed
	case <-ctx.Done():
		// The user canceled the job before it even got to the front of the line
		return ctx.Err()
	}
	defer func() { <-m.sem }() // Give the slot back

	j.progress.Publish(Progress{JobID: j.id, State: Running})
	m.log.Info("job started", "job", j.id, "kind", j.kind, "playlist", j.target)

	// Execute the actual logic
	return run(ctx, func(done, total int, msg string) {
		j.progress.Publish(Progress{JobID: j.id, State: Running, Done: done, Total: total, Message: msg})
	})
}

func (m *Manager) finish(j *job, err error) {
	final := Progress{JobID: j.id, State: Succeeded, Message: "done"}

	switch {
	case errors.Is(err, context.Canceled):
		final.State, final.Message = Canceled, "canceled"
	case err != nil:
		final.State, final.Message, final.Err = Failed, "failed", err.Error()
		m.log.Error("job failed", "job", j.id, "kind", j.kind, "err", err)
	default:
		m.log.Info("job finished", "job", j.id, "kind", j.kind)
	}

	j.progress.Publish(final)

	m.mu.Lock()
	j.final = &final
	j.ended = time.Now()
	delete(m.busy, j.target) // The playlist is free to be edited again
	j.progress.Close()
	m.mu.Unlock()
}

// Subscribe lets the CLI plug into the job's streams
func (m *Manager) Subscribe(id string) (<-chan Progress, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return nil, nil, ErrNotFound
	}

	// If the job already finished but is still within keepHistory, there is no broadcast channel
	// We create a fake channel, send the final result and instantly close it
	if j.final != nil {
		ch := make(chan Progress, 1)
		ch <- *j.final
		close(ch)
		return ch, func() {}, nil
	}

	ch, cancel := j.progress.Subscribe()
	return ch, cancel, nil
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}

	j.cancel()
	return nil
}

func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) evictLocked() {
	for id, j := range m.jobs {
		if j.final != nil && time.Since(j.ended) > keepHistory {
			delete(m.jobs, id)
		}
	}
}
