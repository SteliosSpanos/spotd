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

// This interface allows us to easily swap between a KeyringStore and a FileStore without breaking the app
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
