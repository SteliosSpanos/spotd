// This is known as a Resilient HTTP Client pattern
// 1. Idempotency Rule: Never blindly retry POST requests, only safe methods like GET and PUT
// 2. 4xx (Client Error): Never retry 400, 401, 404
//		429 (Rate Limit): Always retry, but only after the Retry-After header
//		5xx (Server Error): Always retry (if idempotent)
// 3. Backoff + Jitter: Random jitter to avoid crashing the server again after many clients instantly retry
// 4. Never make an HTTP request without a hard timeout or a context

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
	sleep      func(context.Context, time.Duration) error
	baseURL    string
}

// It acts like time.Sleep, but it listens to the context, and if the context is cancelled while it's sleeping, it wakes up and returns the error
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C: // The Timer type represents a single event. When the timer expires, the current time will be sent on C channel
		return nil
	}
}

func New(ts oauth2.TokenSource, l *slog.Logger) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		tokens:     ts,
		log:        l,
		maxRetries: 4,
		baseDelay:  500 * time.Millisecond,
		maxDelay:   30 * time.Second,
		sleep:      sleepCtx,
		baseURL:    baseURL,
	}
}

type APIError struct {
	Status  int
	Message string
	Reason  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("spotify: %d %s", e.Status, e.Message)
}

var ErrNoContent = errors.New("no content")

// PUT is "idempotent" (sending it as many times has the same affect as sending it once)
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/me/player/pause", nil, nil, true)
}

func (c *Client) Next(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/me/player/next", nil, nil, false)
}

// Performs a request with retries
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

		var r io.Reader
		if payload != nil {
			r = bytes.NewReader(payload)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
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
				return ctx.Err() // Cancelled by us, never retry
			}
			// If the action isn't safe to retry or hit the retry limit, then fail
			if !idempotent || attempt >= c.maxRetries {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			// Wait before trying again
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

func (c *Client) handle(resp *http.Response, out any, idempotent bool, attempt int) (bool, time.Duration, error) {
	defer resp.Body.Close()

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
		// Safe to retry only if the action is idempotent
		return idempotent, c.backoff(attempt), parseAPIError(resp.StatusCode, data)
	default:
		// Never retry 4xx. If you request a playlist that doesn't exist retrying won't make it exist
		return false, 0, parseAPIError(resp.StatusCode, data)
	}
}

// Calculates an exponential backoff with jitter
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << attempt
	if d <= 0 || d > c.maxDelay {
		d = c.maxDelay
	}

	// If many users all get disconnected at once, jitter ensures they don't all try to reconnect at the same millisecond and crash the server again
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

func (c *Client) retryAfter(h string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		return min(d, 2*time.Minute)
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
