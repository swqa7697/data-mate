package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// inputReader observes cancellation without leaving a goroutine blocked on stdin.
type inputReader struct {
	ctx  context.Context
	file *os.File
}

func (r inputReader) Read(p []byte) (int, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return r.file.Read(p)
		}
	}
}
func commandInput(cmd *cobra.Command) io.Reader {
	r := cmd.InOrStdin()
	if f, ok := r.(*os.File); ok {
		return inputReader{cmd.Context(), f}
	}
	return r
}
func hasTerminal(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	out, outOK := cmd.ErrOrStderr().(*os.File)
	return ok && outOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

type form struct {
	terminal *term.Terminal
	restore  func() error
	input    io.Reader
	output   io.Writer
}

func openForm(cmd *cobra.Command) (*form, error) { return openFormContext(cmd.Context(), cmd) }
func openFormContext(ctx context.Context, cmd *cobra.Command) (*form, error) {
	if !hasTerminal(cmd) {
		return nil, invalid("complete flags and --yes are required without a terminal")
	}
	f := cmd.InOrStdin().(*os.File)
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, failure("cannot configure terminal")
	}
	rw := struct {
		io.Reader
		io.Writer
	}{terminalInput{inputReader{ctx, f}}, cmd.ErrOrStderr()}
	return &form{term.NewTerminal(rw, ""), func() error { return term.Restore(int(f.Fd()), state) }, rw.Reader, rw.Writer}, nil
}
func (f *form) ask(label, current string, hidden bool) (string, error) {
	var value string
	var err error
	if hidden {
		value, err = f.terminal.ReadPassword(label + ": ")
	} else {
		prompt := label
		if current != "" {
			prompt += " [" + strings.Trim(fmt.Sprintf("%q", current), "\"") + "]"
		}
		f.terminal.SetPrompt(prompt + ": ")
		value, err = f.terminal.ReadLine()
	}
	if err != nil {
		return "", context.Canceled
	}
	if value == "" {
		return current, nil
	}
	return value, nil
}
func (f *form) confirm() error {
	value, err := f.ask("Save changes? [y/N]", "", false)
	if err != nil {
		return err
	}
	if !strings.EqualFold(value, "y") && !strings.EqualFold(value, "yes") {
		return context.Canceled
	}
	return nil
}
func invalid(message string) error { return &Error{ExitInvalid, message} }
func failure(message string) error { return &Error{ExitFailure, message} }

// Terminal reads are single-byte so a form cannot retain a pasted password in
// its line-editor buffer when switching from consent to hidden input.
type terminalInput struct{ io.Reader }

func (r terminalInput) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

// password uses the same raw-mode lifetime as forms, with a mutable, bounded
// buffer. No password is placed in the line editor's immutable string history.
func (f *form) password(label string) ([]byte, error) {
	if _, err := fmt.Fprint(f.output, label+": "); err != nil {
		return nil, vault.ErrUnavailable
	}
	password := make([]byte, 0, vault.MaxSecretBytes)
	success := false
	overflow := false
	defer func() {
		if !success {
			clear(password)
		}
		fmt.Fprint(f.output, "\r\n")
	}()
	var b [1]byte
	for {
		if _, err := io.ReadFull(f.input, b[:]); err != nil {
			return nil, context.Canceled
		}
		switch b[0] {
		case 3, 4:
			return nil, context.Canceled
		case '\r', '\n':
			if overflow || len(password) == 0 || !utf8.Valid(password) {
				return nil, vault.ErrPassword
			}
			success = true
			return password, nil
		case 127, 8:
			if len(password) > 0 {
				_, n := utf8.DecodeLastRune(password)
				clear(password[len(password)-n:])
				password = password[:len(password)-n]
			}
		case 21:
			clear(password)
			password = password[:0]
		default:
			if b[0] < 32 {
				continue
			}
			if overflow {
				continue
			}
			if len(password) == vault.MaxSecretBytes {
				clear(password)
				password = password[:0]
				overflow = true
				continue
			}
			password = append(password, b[0])
		}
	}
}

func keyringPrompt(cmd *cobra.Command) vault.KeyringPrompt {
	return func(ctx context.Context, challenge vault.KeyringChallenge) (password []byte, result error) {
		if !hasTerminal(cmd) {
			return nil, vault.ErrTerminal
		}
		f, err := openFormContext(ctx, cmd)
		if err != nil {
			return nil, vault.ErrTerminal
		}
		defer func() {
			if f.restore() != nil {
				clear(password)
				password = nil
				result = vault.ErrTerminal
			}
		}()
		if challenge.Kind == "unlock" {
			if challenge.Attempt > 1 {
				fmt.Fprintln(f.terminal, "Incorrect keyring password; try again.")
			}
			fmt.Fprintln(f.terminal, "Enter the existing keyring password. It may differ from your Linux login password.")
			return f.password("Keyring password")
		}
		fmt.Fprintln(f.terminal, "No default keyring exists. Create a password-protected Data Mate keyring?")
		consent, err := f.ask("Create keyring? [y/N]", "", false)
		if err != nil || (!strings.EqualFold(consent, "y") && !strings.EqualFold(consent, "yes")) {
			return nil, context.Canceled
		}
		for attempt := 1; attempt <= 3; attempt++ {
			password, err := f.password("New keyring password")
			if errors.Is(err, vault.ErrPassword) {
				fmt.Fprintln(f.terminal, "Enter a nonempty password within the secret-size limit.")
				continue
			}
			if err != nil {
				return nil, err
			}
			confirmation, err := f.password("Confirm keyring password")
			match := bytes.Equal(password, confirmation)
			clear(confirmation)
			if errors.Is(err, vault.ErrPassword) {
				clear(password)
				fmt.Fprintln(f.terminal, "Passwords do not match.")
				continue
			}
			if err != nil {
				clear(password)
				return nil, err
			}
			if match {
				return password, nil
			}
			clear(password)
			fmt.Fprintln(f.terminal, "Passwords do not match.")
		}
		return nil, vault.ErrPassword
	}
}
