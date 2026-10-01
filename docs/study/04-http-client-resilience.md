# 04 — Resilient HTTP client: timeouts, retries, backoff + jitter, 429

## Concepts

**Failure classes** — decide per class whether to retry:

| Failure | Retry? | Notes |
|---------|--------|-------|
| Network error (conn refused/reset, timeout) | yes, if request is safe to repeat | not if `ctx` was cancelled |
| `429 Too Many Requests` | yes, after `Retry-After` | request was **not** processed |
| `500/502/503/504` | yes, idempotent requests only | |
| `401 Unauthorized` | once, after forcing a token refresh | |
| `400/403/404` | no | bug or permission problem; `404 NO_ACTIVE_DEVICE` on player calls is a user-visible state |
| `204 No Content` | — | success, empty body (e.g. nothing playing) |

**Idempotency matters.** `PUT /me/player/pause` twice is harmless. `POST /me/player/next` twice skips two songs. Retry non-idempotent requests only when you know the server did not process them (429, or the connection failed before the request was sent).

**Exponential backoff with full jitter** (AWS architecture blog):
`sleep = random(0, min(cap, base * 2^attempt))`.
Jitter prevents many clients (or your two loops) from retrying in lockstep.

**Retry-After**: on 429 Spotify sends `Retry-After: <seconds>`. Honour it (it beats your own backoff), but cap it and respect `ctx`.

**Timeouts**: every call needs a deadline. Use both an `http.Client.Timeout` (safety net) and a per-call `context.WithTimeout` (precise control).

## Example: thin Spotify client

```go
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

const baseURL = "https://api.spotify.com/v1"

type Client struct {
	http       *http.Client
	tokens     oauth2.TokenSource
	log        *slog.Logger
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	sleep      func(context.Context, time.Duration) error // injectable for tests
	baseURL    string
}

func New(ts oauth2.TokenSource, l *slog.Logger) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{ // explicit, tuned transport
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		tokens: ts, log: l,
		maxRetries: 4, baseDelay: 500 * time.Millisecond, maxDelay: 30 * time.Second,
		sleep: sleepCtx, baseURL: baseURL,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError carries status + Spotify's error message.
type APIError struct {
	Status  int
	Message string
	Reason  string // e.g. "NO_ACTIVE_DEVICE" for player endpoints
}

func (e *APIError) Error() string { return fmt.Sprintf("spotify: %d %s", e.Status, e.Message) }

var ErrNoContent = errors.New("no content")

// do performs a request with retries. body may be nil. out may be nil.
func (c *Client) do(ctx context.Context, method, path string, body any, out any, idempotent bool) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		tok, err := c.tokens.Token()
		if err != nil {
			return fmt.Errorf("get token: %w", err)
		}

		// Body must be recreated each attempt: a reader can only be read once.
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
		if err != nil {
			return err
		}
		tok.SetAuthHeader(req)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // cancelled by us: never retry
			}
			if !idempotent || attempt >= c.maxRetries {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
				return err
			}
			continue
		}

		retry, wait, err := c.handle(resp, out, idempotent, attempt)
		if !retry || attempt >= c.maxRetries {
			return err
		}
		c.log.Warn("retrying spotify request",
			"method", method, "path", path, "status", resp.StatusCode,
			"attempt", attempt+1, "wait", wait)
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// handle consumes and closes the body. Returns whether to retry and how long to wait.
func (c *Client) handle(resp *http.Response, out any, idempotent bool, attempt int) (bool, time.Duration, error) {
	defer resp.Body.Close()
	// Bound what we read: never trust a server to send a sane size.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return idempotent, c.backoff(attempt), err
	}

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return false, 0, ErrNoContent
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || len(data) == 0 {
			return false, 0, nil
		}
		return false, 0, json.Unmarshal(data, out)
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, c.retryAfter(resp.Header.Get("Retry-After"), attempt), parseAPIError(resp.StatusCode, data)
	case resp.StatusCode >= 500:
		return idempotent, c.backoff(attempt), parseAPIError(resp.StatusCode, data)
	default:
		return false, 0, parseAPIError(resp.StatusCode, data)
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << attempt // base * 2^attempt
	if d <= 0 || d > c.maxDelay {
		d = c.maxDelay
	}
	return time.Duration(rand.Int64N(int64(d)) + 1) // full jitter
}

func (c *Client) retryAfter(h string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		return min(d, 2*time.Minute) // cap pathological values
	}
	return c.backoff(attempt)
}

func parseAPIError(status int, data []byte) error {
	var env struct {
		Error struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &env)
	return &APIError{Status: status, Message: env.Error.Message, Reason: env.Error.Reason}
}
```

Typed methods on top:

```go
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/me/player/pause", nil, nil, true)
}

func (c *Client) Next(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/me/player/next", nil, nil, false) // NOT idempotent
}
```

## Handling 401

A 401 usually means the access token was revoked or expired early. Retry **once** after forcing a refresh. `oauth2.ReuseTokenSource` cannot be invalidated, so either:

- keep your own small cache (`mu`, `tok`, `Invalidate()`), or
- rebuild the TokenSource from the stored refresh token.

## Rate-limit budget across loops

Two pollers + user commands share one rate limit (Spotify uses a rolling ~30 s window per app). Options:
- `golang.org/x/time/rate` limiter inside the client (`limiter.Wait(ctx)` before each request).
- A shared "cooldown until" timestamp: when any request gets 429, all requests wait until `now + Retry-After` instead of each discovering 429 independently.

## Testing with httptest

```go
func TestRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var slept []time.Duration
	c := New(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), slog.New(slog.DiscardHandler))
	c.baseURL = srv.URL
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }

	if err := c.Pause(context.Background()); err != nil && !errors.Is(err, ErrNoContent) {
		t.Fatal(err)
	}
	if calls.Load() != 2 || slept[0] != time.Second {
		t.Fatalf("calls=%d slept=%v", calls.Load(), slept)
	}
}
```

Injecting `sleep` keeps tests fast and deterministic. Alternatively use `testing/synctest` (see 18).

## Best practices

- Bound retries (count **and** total time via ctx).
- Never retry when `ctx.Err() != nil`.
- Always close the body, always bound `io.ReadAll`.
- Log each retry at Warn with status, attempt, wait — never the token.
- Return typed errors (`*APIError`, `ErrNoContent`) so callers branch with `errors.As/Is`.
- Consider a circuit breaker only if you can justify it — for a single-user daemon, a shared cooldown is usually enough.

## Pitfalls

- Reusing a consumed `io.Reader` body on retry → empty request body.
- Not draining/closing bodies → connections not reused, leak.
- Retrying `POST /me/player/next` on 5xx → double skip.
- `time.Sleep` in retry loops → ignores cancellation, shutdown hangs.
- Using `math/rand` (v1) global without seeding in old Go; use `math/rand/v2`.

## Further reading

- https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
- https://developer.spotify.com/documentation/web-api/concepts/rate-limits
- https://pkg.go.dev/net/http/httptest
- https://pkg.go.dev/golang.org/x/time/rate
