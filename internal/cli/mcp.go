package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
	mcprelay "github.com/swqa7697/data-mate/internal/mcp"
	"github.com/swqa7697/data-mate/internal/service"
	"github.com/swqa7697/data-mate/internal/vault"
)

func serviceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	for _, safe := range []error{service.ErrState, agent.ErrInspection, config.ErrOwnership, config.ErrPurging, config.ErrStale} {
		if errors.Is(err, safe) {
			return invalid("invalid or unsafe installation/service state")
		}
	}
	for _, safe := range []error{agent.ErrPartial, agent.ErrConflict, service.ErrRestart, service.ErrUnavailable, service.ErrConflict, service.ErrStartup} {
		if errors.Is(err, safe) {
			return failure(safe.Error())
		}
	}
	return failure("service lifecycle operation failed")
}
func serviceController(override string, build Build) (*service.Controller, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, failure("cannot locate executable")
	}
	root, err := config.ResolveRoot(override, exe)
	if err != nil {
		return nil, invalid("invalid installation root")
	}
	b, err := service.ExecutableBuild(build.Version, build.Revision)
	if err != nil {
		return nil, serviceError(err)
	}
	return service.New(root, b), nil
}
func newMCP(override *string, build Build) *cobra.Command {
	mcp := &cobra.Command{Use: "mcp", Short: "Manage background service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, name := range []string{"start", "stop", "status", "bridge"} {
		cmd := &cobra.Command{Use: name, Short: name + " the background service", Hidden: name == "bridge", Args: cobra.NoArgs}
		if name != "bridge" {
			cmd.Flags().Bool("json", false, "Versioned lifecycle and agent readiness report")
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			c, err := serviceController(*override, build)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			if name == "bridge" {
				conn, err := c.OpenSession(ctx)
				if err != nil {
					return serviceError(err)
				}
				input, ok := cmd.InOrStdin().(io.ReadCloser)
				if !ok {
					input = io.NopCloser(cmd.InOrStdin())
				}
				output, ok := cmd.OutOrStdout().(io.WriteCloser)
				if !ok {
					output = bridgeWriter{cmd.OutOrStdout()}
				}
				return serviceError(mcprelay.Bridge(cmd.Context(), conn, input, output))
			}
			c.Agents = agent.New(c.Root)
			var result service.Status
			switch name {
			case "start":
				result, err = c.Start(ctx)
			case "stop":
				result, err = c.Stop(ctx)
				states, inspectErr := c.Agents.Inspect(ctx)
				result.Agents = states
				if err == nil {
					err = inspectErr
				}
			case "status":
				result, err = c.Inspect(ctx)
			}
			// Status retains a machine-readable degraded/stale state even on failure.
			if err != nil && name != "status" && !((name == "start" && result.State == "running") || (name == "stop" && result.State == "stopped")) {
				return serviceError(err)
			}
			var outputErr error
			if flag(cmd, "json") {
				outputErr = json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			} else {
				_, outputErr = fmt.Fprintf(cmd.OutOrStdout(), "Service: %s\nRegistration: %s\nRoot: %s\n", result.State, agent.Name(c.Root), c.Root.Path)
				for _, a := range result.Agents {
					if outputErr == nil {
						_, outputErr = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", a.Name, a.State)
					}
				}
			}
			if outputErr != nil {
				return failure("cannot write service status")
			}
			return serviceError(err)
		}
		mcp.AddCommand(cmd)
	}
	return mcp
}
func internalServiceCommands(override *string, build Build, keys vault.KeyProvider) []*cobra.Command {
	var nonce string
	daemon := &cobra.Command{Use: "__service", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := serviceController(*override, build)
		if err != nil {
			return err
		}
		return serviceError(service.Serve(cmd.Context(), c.Root, c.Build, nonce, keys))
	}}
	daemon.Flags().StringVar(&nonce, "instance", "", "Private service instance")
	install := &cobra.Command{Use: "__install", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		exe, err := os.Executable()
		if err != nil {
			return failure("cannot locate build output")
		}
		root, err := config.ResolveRoot(*override, exe)
		if err != nil {
			return invalid("invalid installation root")
		}
		if filepath.Dir(exe) != filepath.Join(root.Path, "bin") {
			return invalid("build output must be an installation sibling")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 40*time.Second)
		defer cancel()
		store, err := config.Open(ctx, root, nil)
		if err != nil {
			return serviceError(err)
		}
		defer store.Close()
		l, err := store.Lifecycle(ctx)
		if err != nil {
			return serviceError(err)
		}
		defer l.Release()
		if err = l.InstallBinary(filepath.Base(exe)); err != nil {
			return serviceError(err)
		}
		if _, err = l.Read("state/service.json", 4096); err == nil {
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Service restart required: run mcp stop then mcp start.")
			if err != nil {
				return failure("cannot write restart diagnostic")
			}
		}
		return nil
	}}
	return []*cobra.Command{daemon, install}
}

type bridgeWriter struct{ io.Writer }

func (bridgeWriter) Close() error { return nil }
