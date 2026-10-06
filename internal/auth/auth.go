package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// callbackAddr must match the redirect URI registered in the Spotify dashboard exactly.
const callbackAddr = "127.0.0.1:8080"

var spotifyEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.spotify.com/authorize",
	TokenURL:  "https://accounts.spotify.com/api/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

type result struct {
	code string
	err  error
}

func Config(clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    clientID,
		Endpoint:    spotifyEndpoint,
		RedirectURL: "http://" + callbackAddr + "/callback",
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

func trySend(ch chan<- result, r result) {
	select {
	case ch <- r:
	default:
	}
}

func Login(ctx context.Context, conf *oauth2.Config, openBrowser func(string) error) (*oauth2.Token, error) {
	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", callbackAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for callback (is %s free?): %w", callbackAddr, err)
	}

	resCh := make(chan result, 1) // buffered so the handler never blocks
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		// We extract the URL parameters
		q := r.URL.Query()

		// ConstantTimeCompare prevents hackers from guessing the state token via timing attacks
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

		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			trySend(resCh, result{err: errors.New("callback missing code")})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html>
			<head><title>Authentication Successful</title></head>
			<body>
				<h1>Successfully logged in!</h1>
				<p>You can now close this tab and return to the application.</p>
				<script>window.close();</script>
			</body>
		</html>`)
		trySend(resCh, result{code: code})
	})

	// Accepts incoming connections on the listener and creates a new service goroutine for each
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(ln)
	}()
	// Give the server 2 secs to shutdown safely
	defer func() {
		// When we are cleaning up resources we create a new context
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	fmt.Println("Open this URL to log in:\n", authURL)
	_ = openBrowser(authURL)

	// Wait for the user to log in for 5 mins
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for callback: %w", ctx.Err())
	case res := <-resCh: // The backgound web server pushed a result into the channel
		if res.err != nil {
			return nil, res.err
		}

		// We have the code, now we ask Spotify to trade it for an Access Token
		// The oauth2 library is programmed to look inside the ctx object
		// If it finds a custom HTTP client under the key oauth2.HTTPClient, it will use this instead of the default one
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
		tok, err := conf.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange code: %w", err)
		}

		return tok, nil
	}
}

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

type persistingSource struct {
	mu     sync.Mutex // Prevents concurrent writes if multiple HTTP requests refresh at once
	base   oauth2.TokenSource
	lastRT string
	save   func(refreshToken string) error
	logger *slog.Logger
}

// Token is called automatically before every HTTP request
func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if tok.RefreshToken != "" && tok.RefreshToken != p.lastRT {
		// We save the refresh token to the disk so the user doesnt have to open their browser and manually log in every time
		if err := p.save(tok.RefreshToken); err != nil {
			p.logger.Error("persist rotated refresh token", "err", err)
		} else {
			p.lastRT = tok.RefreshToken
		}
	}

	return tok, nil
}

// We pass NewTokenSource to an HTTP client and it guarantees that every HTTP request you make will always have a valid, non-expired token attached to it
func NewTokenSource(ctx context.Context, conf *oauth2.Config, refreshToken string, save func(string) error, l *slog.Logger) oauth2.TokenSource {
	seed := &oauth2.Token{RefreshToken: refreshToken}
	base := conf.TokenSource(ctx, seed)

	return oauth2.ReuseTokenSource(nil, &persistingSource{
		base: base, lastRT: refreshToken, save: save, logger: l,
	})
}
