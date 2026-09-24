package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/service"
)

type diagnosticResult = service.DiagnosticResult

func newDBTest(override *string, factory managementFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "test [alias]", Short: "Check staged connection and read-only transaction readiness", Args: cobra.MaximumNArgs(1)}
	cmd.Flags().Bool("json", false, "Versioned JSON results with reached diagnostic stages")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return runDBTest(cmd, args, *override, factory) }
	return cmd
}
func runDBTest(cmd *cobra.Command, args []string, override string, factory managementFactory) error {
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return failure("cannot locate executable")
	}
	root, err := config.ResolveRoot(override, exe)
	if err != nil {
		return invalid("invalid installation root")
	}
	profiles, _, err := config.Preview(cmd.Context(), root)
	if err != nil {
		return storageError(err, true)
	}
	slices.SortFunc(profiles.Connections, func(a, b config.Profile) int { return strings.Compare(a.Alias, b.Alias) })
	selected := profiles.Connections
	if len(args) > 0 {
		selected = nil
		for _, p := range profiles.Connections {
			if p.Alias == args[0] {
				selected = append(selected, p)
			}
		}
		if len(selected) == 0 {
			return invalid("connection alias not found")
		}
	}
	results := make([]diagnosticResult, 0, len(selected))
	if len(selected) > 0 {
		client, err := factory(cmd.Context(), root)
		if err != nil {
			return serviceError(err)
		}
		defer client.Close()
		for _, p := range selected {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			reply, err := client.Request(cmd.Context(), service.ManagementRequest{Operation: "test", Interactive: hasTerminal(cmd), ProfileID: p.ID, Alias: p.Alias})
			if err != nil {
				return storageError(err, false)
			}
			if reply.Diagnostic == nil {
				return failure("diagnostic response unavailable")
			}
			results = append(results, *reply.Diagnostic)
		}
	}
	// No state lease is retained while writing to potentially slow output.
	if flag(cmd, "json") {
		if err = json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Version int                `json:"version"`
			Results []diagnosticResult `json:"results"`
		}{1, results}); err != nil {
			return failure("cannot write diagnostics")
		}
	} else {
		if len(results) == 0 {
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), "No saved connections."); err != nil {
				return failure("cannot write diagnostics")
			}
		}
		for _, r := range results {
			for _, s := range r.Stages {
				status := "ok"
				if !s.OK {
					status = string(s.Error.Code) + ": " + s.Error.Message
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %s: %s\n", r.Alias, s.Stage, status); err != nil {
					return failure("cannot write diagnostics")
				}
			}
		}
	}
	code := ExitOK
	for _, r := range results {
		if !r.OK {
			if r.Error.Code == contracts.Cancelled {
				return context.Canceled
			}
			code = max(code, ExitFailure)
			if r.Error.Code == contracts.ConfigInvalid {
				code = ExitInvalid
			}
		}
	}
	if code != ExitOK {
		return &Error{code, "one or more connection checks failed"}
	}
	return nil
}
