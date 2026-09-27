package vault

import (
	"context"
	"time"
	"unicode/utf8"
)

// PreparationTimeout includes all terminal input; database budgets start afterward.
const PreparationTimeout = 5 * time.Minute

// KeyringChallenge describes fixed local UI, never provider-supplied text.
// Creation includes consent and password confirmation in the terminal client.
type KeyringChallenge struct {
	Kind    string `json:"kind"`
	Attempt int    `json:"attempt"`
}

func (c KeyringChallenge) Valid() bool {
	return (c.Kind == "unlock" || c.Kind == "create") && c.Attempt >= 1 && c.Attempt <= 3
}

// KeyringPrompt returns a mutable password owned and cleared by the caller.
type KeyringPrompt func(context.Context, KeyringChallenge) ([]byte, error)
type keyringPromptKey struct{}

// WithKeyringPrompt installs the terminal or authenticated management boundary.
func WithKeyringPrompt(ctx context.Context, prompt KeyringPrompt) context.Context {
	return context.WithValue(ctx, keyringPromptKey{}, prompt)
}

// AskKeyring never obtains passwords from environment, command arguments or pipes.
func AskKeyring(ctx context.Context, challenge KeyringChallenge) ([]byte, error) {
	if !challenge.Valid() {
		return nil, ErrUnavailable
	}
	allowed, _ := ctx.Value(interactionKey{}).(bool)
	prompt, _ := ctx.Value(keyringPromptKey{}).(KeyringPrompt)
	if !allowed || prompt == nil {
		return nil, ErrTerminal
	}
	password, err := prompt(ctx, challenge)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (len(password) == 0 || len(password) > MaxSecretBytes || !utf8.Valid(password)) {
		err = ErrPassword
	}
	if err != nil {
		clear(password)
		return nil, err
	}
	return password, nil
}

// PrepareDeletion permits native interaction before any destructive cleanup or
// SQLite transaction. Providers without terminal preparation retain native behavior.
func PrepareDeletion(ctx context.Context, keys KeyProvider, account string) error {
	if p, ok := keys.(interface {
		PrepareDelete(context.Context, string) error
	}); ok {
		ctx, cancel := context.WithTimeout(ctx, PreparationTimeout)
		defer cancel()
		return p.PrepareDelete(ctx, account)
	}
	return nil
}
