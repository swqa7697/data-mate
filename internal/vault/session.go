package vault

import (
	"bytes"
	"context"
	"sync"
)

// SessionKeys retains one installation key only for a service process lifetime.
// It permits loading an existing key, never creation, deletion, or replacement.
// Ciphertext, ledger, bundle binding and profiles are still verified per request.
type SessionKeys struct {
	mu      sync.Mutex
	source  KeyProvider
	account string
	key     []byte
	closed  bool
}

// NewSessionKeys is lazy: an empty service never contacts the native provider.
func NewSessionKeys(source KeyProvider) *SessionKeys { return &SessionKeys{source: source} }

// Load returns an independent buffer which the caller must clear after use.
func (s *SessionKeys) Load(ctx context.Context, account string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed || (s.account != "" && s.account != account) {
		return nil, ErrUnavailable
	}
	if s.key == nil {
		key, err := s.source.Load(ctx, account)
		if err != nil {
			return nil, providerError(err)
		}
		if len(key) != 32 {
			clear(key)
			return nil, ErrRepair
		}
		if err = ctx.Err(); err != nil {
			clear(key)
			return nil, err
		}
		s.key = key
		s.account = account
	}
	return bytes.Clone(s.key), nil
}

// CreateIfAbsent refuses credential mutations through a service session.
func (s *SessionKeys) CreateIfAbsent(context.Context, string, []byte) ([]byte, error) {
	return nil, ErrUnavailable
}

// Delete refuses credential mutations through a service session.
func (s *SessionKeys) Delete(context.Context, string) error { return ErrUnavailable }

// Close releases the process key and prevents later loads.
func (s *SessionKeys) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	clear(s.key)
	s.key = nil
}
