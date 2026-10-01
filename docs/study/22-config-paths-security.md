# 22 — Config, file paths & security checklist

## Where files go (XDG on Linux, equivalents elsewhere)

| What | Linux | macOS | Go helper |
|------|-------|-------|-----------|
| Config (`config.toml`, client ID) | `$XDG_CONFIG_HOME/spotd` (`~/.config/spotd`) | `~/Library/Application Support/spotd` | `os.UserConfigDir()` |
| Data (SQLite DB, backups) | `$XDG_DATA_HOME/spotd` (`~/.local/share/spotd`) | `~/Library/Application Support/spotd` | none in stdlib — read env, fall back |
| State / logs | `$XDG_STATE_HOME/spotd` (`~/.local/state/spotd`) | `~/Library/Logs/spotd` | none in stdlib |
| Cache | `$XDG_CACHE_HOME/spotd` | `~/Library/Caches/spotd` | `os.UserCacheDir()` |
| Socket | `$XDG_RUNTIME_DIR/spotd/spotd.sock` | data dir or `$TMPDIR` | none |
| Secrets | **OS keyring** | **OS keyring** | go-keyring |

`github.com/adrg/xdg` implements all of these cross-platform if you prefer a library.

```go
func dataDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "spotd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "spotd"), nil
	}
	return filepath.Join(home, ".local", "share", "spotd"), nil
}
```

Create dirs with `os.MkdirAll(dir, 0o700)`; files with `0o600`.

## Configuration precedence

`flags > env (SPOTD_*) > config file > defaults`. Keep it small: client ID, poll intervals, socket path, log level/format. Validate on startup and fail fast with a clear message (e.g. fast interval < 1 s rejected — protects your rate limit).

The Spotify **client ID is not a secret** in PKCE (public client), so it can live in the config file or be passed by flag. There is no client secret at all.

## Threat model (short — put a version in your README)

| Asset | Threat | Mitigation |
|-------|--------|------------|
| Refresh token | other local users / backups / dotfile repos | OS keyring, never written to disk or logs |
| Access token | log leakage | memory only; `slog` redaction + `Secret` LogValuer |
| OAuth callback | CSRF / code injection | random `state`, constant-time compare, PKCE S256, bind `127.0.0.1` only, single-use server, timeout |
| Daemon control socket | other local users controlling playback / reading history | socket in `0700` dir, socket `0600`; optional `SO_PEERCRED` UID check |
| SQLite DB (listening history = personal data) | other local users | `0700` data dir, `0600` file |
| Spotify API | bearer token sent to wrong host | only follow pagination URLs on `api.spotify.com` |
| Backup/restore files | malformed/malicious JSON | size limit, strict decoding (`DisallowUnknownFields` optional), validate URIs, fuzz the decoder |
| gRPC inputs | invalid URIs, huge limits | validate & clamp in handlers → `InvalidArgument` |
| Dependencies | known CVEs | `govulncheck` in CI, Dependabot |
| CI | supply chain | least-privilege `permissions`, pinned actions, no `pull_request_target` |

## Security checklist

- [ ] No token, auth code, or full callback URL in logs (test it: grep logs in a test).
- [ ] Every outbound HTTP call has a timeout; response bodies are size-limited.
- [ ] `crypto/rand` for `state` and job IDs; `math/rand/v2` only for jitter.
- [ ] SQL only via sqlc parameters — no string-built SQL.
- [ ] File perms: dirs `0700`, files `0600`, socket `0600`. Note `umask` can only *remove* bits, so set them explicitly.
- [ ] Atomic writes (temp file + `os.Rename`) for backups/config.
- [ ] Destructive playlist jobs make a backup first.
- [ ] `invalid_grant` on refresh → clear guidance to re-login, no infinite retry loop.
- [ ] `spotd logout` removes the keyring entry.
- [ ] gosec + govulncheck clean.

## Further reading

- https://specifications.freedesktop.org/basedir-spec/latest/
- https://owasp.org/www-project-top-ten/ and OWASP ASVS
- RFC 8252 (OAuth 2.0 for Native Apps), RFC 9700 (OAuth 2.0 Security Best Current Practice)
- https://go.dev/doc/security/best-practices
