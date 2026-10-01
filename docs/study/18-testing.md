# 18 — Testing strategy

"Thorough testing" is a stated goal, so tests are a feature. Aim to *prove the hard claims* in your README: non-blocking broadcast, idempotent ingest, retry on 429, clean shutdown.

## Test pyramid for spotd

| Layer | What | Tools |
|-------|------|-------|
| Unit | `changed()`, backoff math, status mapping, DTO decoding, TUI `Update` | table tests, fakes |
| Component | Spotify client vs fake server; store vs real SQLite; broadcaster under load | `httptest`, temp-file SQLite, `-race` |
| Integration | gRPC server + fakes, TUI client stubs | `bufconn` / real UDS |
| Timing | poll loops, backoff, shutdown timeouts | `testing/synctest` |
| Property / fuzz | parsers, `Retry-After`, URI validation | `go test -fuzz` |

## Table-driven tests

```go
func TestChanged(t *testing.T) {
	t0 := time.Unix(0, 0)
	base := &NowPlaying{TrackID: "a", IsPlaying: true, ProgressMs: 10_000, FetchedAt: t0}

	tests := []struct {
		name string
		prev *NowPlaying
		cur  *NowPlaying
		want bool
	}{
		{"nothing to nothing", nil, nil, false},
		{"start playing", nil, base, true},
		{"stop", base, nil, true},
		{"same track progressing normally", base, with(base, func(n *NowPlaying) {
			n.ProgressMs, n.FetchedAt = 15_000, t0.Add(5*time.Second)
		}), false},
		{"seek", base, with(base, func(n *NowPlaying) {
			n.ProgressMs, n.FetchedAt = 90_000, t0.Add(5*time.Second)
		}), true},
		{"paused", base, with(base, func(n *NowPlaying) { n.IsPlaying = false }), true},
		{"track change", base, with(base, func(n *NowPlaying) { n.TrackID = "b" }), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changed(tt.prev, tt.cur, 5*time.Second); got != tt.want {
				t.Errorf("changed() = %v, want %v", got, tt.want)
			}
		})
	}
}
```

## Fakes over mocks

Define small interfaces at the consumer (01) and write hand-rolled fakes:

```go
type fakeSpotify struct {
	mu       sync.Mutex
	playing  []*spotify.Playing // scripted responses, popped per call
	recent   [][]spotify.PlayHistory
	calls    int
}
```

Fakes are readable, refactor-friendly, and don't need a mocking framework. If you do want generated mocks, `go.uber.org/mock` is the maintained fork of gomock.

## HTTP: httptest

See 04. Also test:
- 5xx on POST is **not** retried.
- Context cancellation during backoff returns promptly.
- Oversized body is bounded.
- Decoding of recorded fixtures under `testdata/` (strip personal data).

## Database: real SQLite

```go
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { db.Close() })
	if err := Migrate(ctx, db.W, slog.New(slog.DiscardHandler)); err != nil { t.Fatal(err) }
	return New(db)
}

func TestInsertPlaysIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	items := fixturePlays(t, 10)
	n1, _ := s.InsertPlays(t.Context(), items)
	n2, _ := s.InsertPlays(t.Context(), items[5:]) // overlap
	if n1 != 10 || n2 != 0 {
		t.Fatalf("n1=%d n2=%d", n1, n2)
	}
}
```

`t.Context()` (Go 1.24+) is cancelled when the test ends.

## Concurrency: -race and goleak

```go
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```

Run `go test -race -count=1 ./...` locally and in CI. Use `-count=N` / `-shuffle=on` to flush out flaky order-dependent tests.

## Time: testing/synctest (Go 1.25+)

`synctest.Test` runs code in a "bubble" with a fake clock: time only advances when every goroutine in the bubble is blocked. A 3-minute ticker test runs instantly and deterministically.

```go
import "testing/synctest"

func TestSlowLoopIngestsEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &fakeSpotify{ /* ... */ }
		store := &fakeStore{}
		p := New(api, store, nopPublisher{}, slog.New(slog.DiscardHandler))
		p.slowEvery = 3 * time.Minute

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- p.RunSlow(ctx) }()

		synctest.Wait() // initial ingest finished, loop now blocked on ticker
		if store.ingests() != 1 { t.Fatalf("want 1 ingest at start") }

		time.Sleep(3 * time.Minute) // instant inside the bubble
		synctest.Wait()
		if store.ingests() != 2 { t.Fatalf("want 2 ingests") }

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) { t.Fatal(err) }
	})
}
```

Limits: real network I/O (httptest over TCP) is not "durably blocking", so inside a bubble use in-memory fakes (or `net.Pipe`). Read the package docs before using it.

## gRPC

`bufconn` (see 13). Test status codes, streaming delivery, and that cancelling the client stream ends the server handler (no leaked subscription: assert broadcaster subscriber count returns to 0).

## Fuzzing

```go
func FuzzRetryAfter(f *testing.F) {
	for _, s := range []string{"0", "1", "120", "-1", "", "abc", "99999999999"} {
		f.Add(s)
	}
	c := testClient()
	f.Fuzz(func(t *testing.T, h string) {
		d := c.retryAfter(h, 0)
		if d < 0 || d > 2*time.Minute {
			t.Fatalf("retryAfter(%q) = %v out of bounds", h, d)
		}
	})
}
```

Good fuzz targets: `Retry-After` parsing, Spotify URI validation, backup-file decoding (restore reads files from disk — untrusted input).

## Golden files

For TUI `View()` and CLI output: render, compare to `testdata/*.golden`, regenerate with `-update` flag.

```go
var update = flag.Bool("update", false, "update golden files")
```

## Coverage

```
go test -race -coverprofile=cover.out ./...
go tool cover -html=cover.out
```

Report coverage in CI, but don't chase 100%. Excluding generated code (`gen/`, `internal/store/db/`) keeps the number honest.

## Best practices

- Tests run with `go test ./...` and no network, no keyring, no Spotify account.
- `t.Parallel()` where tests are independent (temp dirs make DB tests parallel-safe).
- `t.Helper()` in helpers; `t.Cleanup` over `defer` in helpers.
- Name tests by behaviour: `TestPublish_DoesNotBlockOnSlowSubscriber`.
- Optional manual E2E: build tag `//go:build e2e` test hitting real Spotify with your account, never run in CI.

## Further reading

- https://go.dev/doc/tutorial/add-a-test, https://go.dev/wiki/TableDrivenTests
- https://pkg.go.dev/testing/synctest and https://go.dev/blog/synctest
- https://go.dev/doc/security/fuzz/
- https://pkg.go.dev/google.golang.org/grpc/test/bufconn
- https://github.com/uber-go/goleak
