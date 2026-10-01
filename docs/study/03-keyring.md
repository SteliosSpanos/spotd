# 03 — OS keyring with zalando/go-keyring

## Concepts

The OS keyring stores secrets encrypted at rest, unlocked with the user's login:

| OS | Backend used by go-keyring |
|----|----------------------------|
| macOS | Keychain (via `/usr/bin/security`) |
| Linux/BSD | Secret Service API over D-Bus (GNOME Keyring, KWallet ≥ 5.97, KeePassXC) |
| Windows | Credential Manager |

Why not a file: a `~/.config/spotd/token.json` with `0600` is readable by any process running as you and ends up in backups/dotfile repos. The keyring is the expected place for long-lived credentials.

## API

```go
import "github.com/zalando/go-keyring"

keyring.Set(service, user, secret string) error
keyring.Get(service, user string) (string, error)   // keyring.ErrNotFound if missing
keyring.Delete(service, user string) error
keyring.MockInit()                                   // in-memory backend for tests
```

## Example: a small store behind an interface

```go
package auth

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const (
	service = "spotd"
	account = "spotify-refresh-token"
)

var ErrNotLoggedIn = errors.New("not logged in: run `spotd login`")

// SecretStore is what the rest of the code depends on; easy to fake.
type SecretStore interface {
	SaveRefreshToken(string) error
	RefreshToken() (string, error)
	Clear() error
}

type KeyringStore struct{}

func (KeyringStore) SaveRefreshToken(t string) error {
	if err := keyring.Set(service, account, t); err != nil {
		return fmt.Errorf("keyring set: %w", err)
	}
	return nil
}

func (KeyringStore) RefreshToken() (string, error) {
	t, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotLoggedIn
	}
	if err != nil {
		return "", fmt.Errorf("keyring get: %w", err)
	}
	return t, nil
}

func (KeyringStore) Clear() error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
```

Tests:

```go
func TestKeyringRoundTrip(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	if _, err := s.RefreshToken(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
	if err := s.SaveRefreshToken("abc"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.RefreshToken()
	if got != "abc" {
		t.Fatalf("got %q", got)
	}
}
```

## Best practices

- Store **only** the refresh token. Access tokens stay in memory.
- Store just the string; if you need more fields later, store small JSON — but keep it small (Windows Credential Manager limits secret size to ~2.5 KB; macOS has size limits too).
- Add a `spotd logout` that calls `Clear()`.
- Wrap errors with a hint for headless Linux: "no Secret Service provider found — install/unlock gnome-keyring or KeePassXC".
- In CI, call `keyring.MockInit()` in tests; CI runners have no D-Bus session.

## Pitfalls

- Linux servers / WSL / SSH sessions without a D-Bus session bus: `Set` fails. Decide explicitly whether to support a fallback (e.g. an encrypted file) — the spec says "never plain files", so failing with a clear message is the honest choice.
- macOS backend shells out to `security`; the secret briefly appears in that process's arguments. Acceptable for a personal tool, worth knowing.
- Concurrent writers: only the daemon should write after login. If `login` runs while the daemon runs, the daemon should re-read the keyring on `invalid_grant` before giving up.

## Further reading

- https://github.com/zalando/go-keyring
- https://specifications.freedesktop.org/secret-service/
