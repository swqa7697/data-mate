// Package vault encrypts credential bundles and coordinates durable profile writes.
package vault

import (
	"context"
	"errors"
)

var (
	ErrMissing             = errors.New("credential key is missing; restore the original keyring or repair the installation")
	ErrDenied              = errors.New("credential store access denied")
	ErrLocked              = errors.New("credential store is locked; rerun with terminal input and stderr or unlock the provider using its native interface")
	ErrUnavailable         = errors.New("credential store unavailable")
	ErrProviderUnavailable = errors.New("secret service provider unavailable; ensure a credential provider is installed and available in your user session")
	ErrPassword            = errors.New("incorrect keyring password")
	ErrTerminal            = errors.New("keyring authentication requires terminal input and stderr; rerun this command in a terminal (over SSH, allocate a TTY)")
	ErrUserBus             = errors.New("user D-Bus is unavailable; connect through a login session with a running user bus")
	ErrUnsupported         = errors.New("credential provider does not support terminal keyring setup or unlocking; unlock it using its native interface")
	ErrProviderChanged     = errors.New("credential provider changed during authentication; retry the command")
	ErrRepair              = errors.New("vault or usage record requires repair")
	ErrLimit               = errors.New("vault encryption limit reached; use a new installation namespace")
	ErrCredentialMissing   = errors.New("credential bundle missing; repair credentials with db edit")
	ErrBinding             = errors.New("credential bundle belongs to another connection")
)

// KeyProvider is the native boundary. CreateIfAbsent returns the existing key on
// duplicate; it must never overwrite an item. Delete is exact and idempotent.
// Implementations must return only these safe errors, never upstream diagnostics.
type KeyProvider interface {
	Load(context.Context, string) ([]byte, error)
	CreateIfAbsent(context.Context, string, []byte) ([]byte, error)
	Delete(context.Context, string) error
}

func providerError(err error) error {
	for _, safe := range []error{ErrMissing, ErrDenied, ErrLocked, ErrUnavailable, ErrPassword, ErrTerminal, ErrUserBus, ErrUnsupported, ErrProviderChanged, ErrProviderUnavailable, ErrRepair, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return ErrUnavailable
}

type interactionKey struct{}

// WithInteraction explicitly authorizes or forbids prompting for this operation.
func WithInteraction(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, interactionKey{}, allowed)
}
