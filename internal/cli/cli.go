// Package cli owns command parsing, user output, and process exit contracts.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/vault"
)

const (
	ExitOK        = 0
	ExitFailure   = 1
	ExitInvalid   = 2
	ExitCancelled = 130
)

// Build contains metadata injected from VERSION by the build script.
type Build struct{ Version, Revision, Dirty string }

// Error carries only a safe public diagnostic and process status.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// ExitCode maps cancellation, input, and operational failures to stable statuses.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitFailure
}

// Run executes with explicit streams; Cobra never prints untrusted argument values.
func Run(ctx context.Context, args []string, out, stderr io.Writer, build Build) int {
	root := New(build)
	root.SetOut(out)
	root.SetErr(stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil {
		var e *Error
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "cancelled")
		} else if errors.As(err, &e) {
			fmt.Fprintln(stderr, e.Message)
		} else {
			fmt.Fprintln(stderr, "invalid command or arguments; run data-mate help")
			return ExitInvalid
		}
	}
	return ExitCode(err)
}

// New creates the CLI without touching configuration, credentials, or agents.
func New(build Build) *cobra.Command { return newCommand(build, nil) }

func newCommand(build Build, keys vault.KeyProvider) *cobra.Command {
	return commandWithDatabase(build, keys, defaultDatabase)
}
func commandWithDatabase(build Build, keys vault.KeyProvider, factory databaseFactory) *cobra.Command {
	var override string
	root := &cobra.Command{Use: "data-mate", Short: "Checkout-local PostgreSQL access for terminal agents", SilenceErrors: true, SilenceUsage: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", Short: "Show command help", RunE: func(_ *cobra.Command, args []string) error {
		target, remaining, err := root.Find(args)
		if err != nil || len(remaining) != 0 {
			return &Error{ExitInvalid, "invalid help topic; run data-mate help"}
		}
		return target.Help()
	}})
	root.PersistentFlags().StringVar(&override, "root", "", "Absolute installation root (defaults to executable location)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error { return &Error{ExitInvalid, "invalid flags; run data-mate help"} })
	root.AddCommand(&cobra.Command{Use: "version", Short: "Show application version and build metadata", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "data-mate %s (revision %s, dirty %s)\n", build.Version, build.Revision, build.Dirty)
		if err != nil {
			return &Error{ExitFailure, "cannot write output"}
		}
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "upgrade", Aliases: []string{"update"}, Short: "Explain local rebuilding", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Distribution is deferred. Rebuild from your checkout with make install.")
		if err != nil {
			return &Error{ExitFailure, "cannot write output"}
		}
		return nil
	}})
	db := newDB(&override, keys, factory)
	root.AddCommand(db, newMCP(&override, build))
	root.AddCommand(internalServiceCommands(&override, build, keys)...)
	return root
}
