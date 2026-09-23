// Package vault encrypts credential bundles and coordinates durable profile writes.
package vault

import (
	"context"
	"errors"
)

var (
	ErrMissing           = errors.New("credential key is missing")
	ErrDenied            = errors.New("credential store access denied")
	ErrLocked            = errors.New("credential store interaction required or locked")
	ErrUnavailable       = errors.New("credential store unavailable")
	ErrRepair            = errors.New("vault or usage record requires repair")
	ErrLimit             = errors.New("vault encryption limit reached; use a new installation namespace")
	ErrCredentialMissing = errors.New("credential bundle missing; repair credentials with db edit")
	ErrBinding           = errors.New("credential bundle belongs to another connection")
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
	for _, safe := range []error{ErrMissing, ErrDenied, ErrLocked, ErrUnavailable, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return ErrUnavailable
}
