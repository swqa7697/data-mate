package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
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
}

func openForm(cmd *cobra.Command) (*form, error) {
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
	}{commandInput(cmd), cmd.ErrOrStderr()}
	return &form{term.NewTerminal(rw, ""), func() error { return term.Restore(int(f.Fd()), state) }}, nil
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
