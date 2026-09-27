package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func pageDescription(cmd *cobra.Command, in *os.File, text string) error {
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	state, err := term.GetState(int(in.Fd()))
	if err != nil {
		_, err = io.WriteString(cmd.OutOrStdout(), text)
		return err
	}
	// CommandContext may kill the pager during cancellation, before less can
	// restore its own terminal settings. Always restore the captured state.
	defer func() { _ = term.Restore(int(in.Fd()), state) }()
	pager := exec.CommandContext(cmd.Context(), "/usr/bin/less", "-R", "-F", "-X")
	pager.Stdin = strings.NewReader(text)
	pager.Stdout = cmd.OutOrStdout()
	pager.Stderr = cmd.ErrOrStderr()
	// Give less a chance to reset terminal escape modes as well as termios.
	// WaitDelay bounds graceful shutdown and escalates to a kill if necessary.
	pager.Cancel = func() error { return pager.Process.Signal(syscall.SIGTERM) }
	pager.WaitDelay = time.Second
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "LESS") && key != "PAGER" {
			pager.Env = append(pager.Env, entry)
		}
	}
	// Do not invoke shell commands, load personal key bindings, or save searches.
	pager.Env = append(pager.Env, "LESSSECURE=1", "LESSCHARSET=utf-8", "LESSHISTFILE=-", "LESSKEYIN=/dev/null", "LESSKEYIN_SYSTEM=/dev/null")
	if err = pager.Start(); err != nil {
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		_, err = io.WriteString(cmd.OutOrStdout(), text)
		return err
	}
	err = pager.Wait()
	if cmd.Context().Err() != nil {
		return cmd.Context().Err()
	}
	// A successful early quit can close stdin while os/exec is still copying.
	if errors.Is(err, syscall.EPIPE) {
		return nil
	}
	return err
}
