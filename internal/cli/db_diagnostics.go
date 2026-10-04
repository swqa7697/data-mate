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
	"github.com/swqa7697/data-mate/internal/vault"
)

type diagnosticResult = service.DiagnosticResult

func newDBTest(override *string, factory managementFactory, build Build) *cobra.Command {
	cmd := &cobra.Command{Use: "test [alias]", Short: "Check staged connection and read-only transaction readiness", Args: cobra.MaximumNArgs(1)}
	cmd.Flags().Bool("json", false, "Versioned JSON results with reached diagnostic stages")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return runDBTest(cmd, args, *override, factory, build) }
	return cmd
}
func runDBTest(cmd *cobra.Command, args []string, override string, factory managementFactory, build Build) error {
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return failure("cannot locate executable")
	}
	root, err := build.resolveRoot(override, exe)
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
	aliases := make([]string, len(selected))
	targets := make([]service.ProfileTarget, len(selected))
	for i, p := range selected {
		aliases[i] = p.Alias
		targets[i] = service.ProfileTarget{ProfileID: p.ID, Alias: p.Alias}
	}
	board := newDiagnosticBoard(aliases)
	results := []diagnosticResult{}
	if len(selected) > 0 {
		var display *diagnosticDisplay
		if !flag(cmd, "json") {
			display = startDiagnosticDisplay(cmd.OutOrStdout(), board)
		}
		err = requestDiagnostics(cmd, root, factory, board, targets, display)
		if display != nil {
			if e := display.stop(); e != nil && err == nil {
				err = failure("cannot write diagnostics")
			}
		}
		if err != nil {
			return err
		}
		var complete bool
		if results, complete = board.complete(); !complete {
			return failure("diagnostic response unavailable")
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
	} else if len(results) == 0 {
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), "No saved connections."); err != nil {
			return failure("cannot write diagnostics")
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

// requestDiagnostics submits every target in one management request. The
// service checks them concurrently and the board receives each result as it
// completes; terminal prompts pause the live display.
func requestDiagnostics(cmd *cobra.Command, root config.Root, factory managementFactory, board *diagnosticBoard, targets []service.ProfileTarget, display *diagnosticDisplay) error {
	client, err := factory(cmd.Context(), root)
	if err != nil {
		return serviceError(err)
	}
	defer client.Close()
	ctx := cmd.Context()
	if display != nil && display.live {
		prompt := keyringPrompt(cmd)
		ctx = vault.WithKeyringPrompt(ctx, func(ctx context.Context, challenge vault.KeyringChallenge) ([]byte, error) {
			display.pause()
			defer display.resume()
			return prompt(ctx, challenge)
		})
	}
	if _, err = client.Request(ctx, service.ManagementRequest{Operation: "test", Interactive: hasTerminal(cmd), Targets: targets, Report: board.report}); err != nil {
		return storageError(err, false)
	}
	return nil
}
