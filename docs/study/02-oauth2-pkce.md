# 02 — OAuth 2.0 Authorization Code + PKCE

## Concepts

**Authorization Code flow**: the user authenticates on Spotify's site, Spotify redirects back to your `redirect_uri` with a short-lived `code`, and you exchange that code for tokens at the token endpoint.

**PKCE (RFC 7636)** makes this safe for *public clients* (CLI/desktop apps that cannot keep a client secret):

1. Generate a random `code_verifier` (43–128 chars, high entropy).
2. Derive `code_challenge = BASE64URL(SHA256(code_verifier))`.
3. Send the **challenge** in the authorize URL; send the **verifier** in the token exchange.
4. Anyone who intercepts the `code` cannot exchange it without the verifier.

**`state`** parameter: random value you send and expect back unchanged — defends against CSRF / mixing up login attempts.

**Tokens**

| Token | Lifetime | Where it lives |
|-------|----------|----------------|
| access token | ~1 hour | memory only |
| refresh token | long-lived, **may rotate** on each refresh | OS keyring |

Spotify with PKCE may return a **new refresh token** on refresh. The old one may stop working. You must persist the new one — this is the "persists rotated refresh tokens" requirement.

## Spotify specifics

- Authorize: `https://accounts.spotify.com/authorize`
- Token: `https://accounts.spotify.com/api/token`
- Redirect URI must be registered in the dashboard **exactly**: `http://127.0.0.1:8080/callback`. Spotify rejects `localhost`; loopback IP literals are allowed over plain HTTP.
- PKCE: no client secret; `client_id` is sent in the request body (`AuthStyleInParams`).
- Scopes for spotd (request the minimum):
  - `user-read-currently-playing`, `user-read-playback-state`
  - `user-modify-playback-state` (play/pause/skip/queue — needs Premium)
  - `user-read-recently-played`, `user-top-read`
  - `playlist-read-private`, `playlist-read-collaborative`, `playlist-modify-private`, `playlist-modify-public`

## Example with golang.org/x/oauth2

`x/oauth2` has built-in PKCE helpers: `GenerateVerifier`, `S256ChallengeOption`, `VerifierOption`.

```go
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

var spotifyEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.spotify.com/authorize",
	TokenURL:  "https://accounts.spotify.com/api/token",
	AuthStyle: oauth2.AuthStyleInParams, // public client: client_id in body, no secret
}

func Config(clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    clientID,
		Endpoint:    spotifyEndpoint,
		RedirectURL: "http://127.0.0.1:8080/callback",
		Scopes: []string{
			"user-read-currently-playing", "user-read-playback-state",
			"user-modify-playback-state", "user-read-recently-played",
			"user-top-read", "playlist-read-private",
			"playlist-read-collaborative", "playlist-modify-private",
			"playlist-modify-public",
		},
	}
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type result struct {
	code string
	err  error
}

// Login runs the full PKCE flow and returns the token.
func Login(ctx context.Context, conf *oauth2.Config, openBrowser func(string) error) (*oauth2.Token, error) {
	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	// Bind explicitly to the loopback IP, never 0.0.0.0.
	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		return nil, fmt.Errorf("listen for callback (is port 8080 free?): %w", err)
	}

	resCh := make(chan result, 1) // buffered: handler never blocks
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			trySend(resCh, result{err: errors.New("state mismatch")})
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization denied", http.StatusBadRequest)
			trySend(resCh, result{err: fmt.Errorf("authorization error: %s", e)})
			return
		}
		fmt.Fprintln(w, "Logged in. You can close this tab.")
		trySend(resCh, result{code: q.Get("code")})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	fmt.Println("Open this URL to log in:\n", authURL)
	_ = openBrowser(authURL) // best effort; URL is printed anyway

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for callback: %w", ctx.Err())
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		// Use an HTTP client with a timeout for the exchange.
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
		tok, err := conf.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange code: %w", err)
		}
		return tok, nil
	}
}

func trySend(ch chan<- result, r result) {
	select {
	case ch <- r:
	default: // already have a result; ignore duplicates/refreshes of the tab
	}
}
```

Opening the browser cross-platform:

```go
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
```

## Persisting rotated refresh tokens (daemon side)

`oauth2.Config.TokenSource(ctx, tok)` refreshes automatically, but it does not tell you when the refresh token changed. Wrap it:

```go
// persistingSource saves the token whenever the refresh token changes.
type persistingSource struct {
	mu      sync.Mutex
	base    oauth2.TokenSource
	lastRT  string
	save    func(refreshToken string) error
	logger  *slog.Logger
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.RefreshToken != "" && tok.RefreshToken != p.lastRT {
		if err := p.save(tok.RefreshToken); err != nil {
			// Do not fail the request: the in-memory token still works.
			// But log loudly — if we restart now we may lose access.
			p.logger.Error("persist rotated refresh token", "err", err)
		} else {
			p.lastRT = tok.RefreshToken
		}
	}
	return tok, nil
}

func NewTokenSource(ctx context.Context, conf *oauth2.Config, refreshToken string, save func(string) error, l *slog.Logger) oauth2.TokenSource {
	seed := &oauth2.Token{RefreshToken: refreshToken} // expired access token forces a refresh
	base := conf.TokenSource(ctx, seed)             // already a ReuseTokenSource
	return oauth2.ReuseTokenSource(nil, &persistingSource{
		base: base, lastRT: refreshToken, save: save, logger: l,
	})
}
```

Notes:
- `conf.TokenSource` keeps the old refresh token if the server does not return a new one.
- The `ctx` passed to `conf.TokenSource` is used for refresh HTTP calls — give it a client with a timeout via `oauth2.HTTPClient`, and make it a long-lived context (not a request context).
- `oauth2.NewClient(ctx, ts)` gives an `*http.Client` that injects `Authorization: Bearer`. For spotd you may prefer to call `ts.Token()` yourself inside your own client so retries/401 handling stay in one place (see 04).

## Best practices

- Always PKCE + `state`. Use `S256`, never `plain`.
- Bind to `127.0.0.1`, single-use server, shut it down right after.
- Timeout the whole login (5 min) and the token exchange (15 s).
- Never log tokens, codes, or the full callback URL. Redact in `slog` (see 15).
- Request minimal scopes; document why each is needed.
- If refresh returns `invalid_grant`, the refresh token is dead: surface "run `spotd login` again" instead of retrying forever.

## Pitfalls

- Registering `http://localhost:8080/callback` (rejected) or a trailing-slash mismatch.
- Port 8080 already used → clear error message.
- Unbuffered result channel + user refreshing the callback tab → handler goroutine blocks forever.
- Losing a rotated refresh token because you only saved on login.
- Using `context.Background()` for the TokenSource but a default `http.Client` with no timeout → a hung refresh hangs everything.

## Further reading

- RFC 7636 (PKCE), RFC 6749 §4.1, RFC 8252 (OAuth for native apps — loopback redirect guidance)
- https://developer.spotify.com/documentation/web-api/tutorials/code-pkce-flow
- https://pkg.go.dev/golang.org/x/oauth2
