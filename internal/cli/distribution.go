package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/distribution"
	"github.com/swqa7697/data-mate/internal/service"
)

func distributionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var conflict *distribution.ArtifactError
	if errors.As(err, &conflict) {
		return failure(conflict.Error())
	}
	var directory *distribution.DirectoryError
	if errors.As(err, &directory) {
		return failure(directory.Error())
	}
	if message := keyringError(err); message != "" {
		return failure(message)
	}
	var cleanup *service.CleanupError
	if errors.As(err, &cleanup) {
		return failure(cleanup.Error() + "; retry uninstall with the same options")
	}
	if errors.Is(err, service.ErrUnavailable) {
		return failure("cannot inspect the per-user service manager for this installation's recorded service; retry when the manager is available")
	}
	// Preserve known nonsecret causes without printing wrapped OS, subprocess,
	// or storage errors, which can contain private data.
	for _, safe := range []error{distribution.ErrRelease, distribution.ErrConflict, config.ErrOwnership, config.ErrState, config.ErrPending, config.ErrPurging, config.ErrStale, service.ErrState, service.ErrConflict} {
		if errors.Is(err, safe) {
			return failure("distribution operation incomplete: " + safe.Error())
		}
	}

	var path *os.PathError
	if errors.As(err, &path) {
		return failure(fmt.Sprintf("%s %q: %v", path.Op, path.Path, path.Err))
	}
	return failure("distribution operation incomplete; retry the command")
}

func distributionEngine(ctx context.Context, build Build, rootCommand *cobra.Command) (*distribution.Engine, error) {
	root, err := build.resolveRoot("", "")
	if err != nil {
		return nil, err
	}
	homeResolver := build.accountHome
	if homeResolver == nil {
		homeResolver = config.AccountHome
	}
	home, err := homeResolver()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	nativeBuild, err := service.ExecutableBuild(build.Version, build.Revision)
	if err != nil {
		return nil, err
	}
	controller := service.New(root, nativeBuild)
	controller.Interactive = hasTerminal(rootCommand)
	engine := &distribution.Engine{Home: home, Root: root, Metadata: distribution.Contract(build.Version), Candidate: exe, ZDotDir: os.Getenv("ZDOTDIR")}
	engine.Completion = func(shell string) ([]byte, error) {
		var out bytes.Buffer
		err := writeCompletion(rootCommand, shell, &out)
		return out.Bytes(), err
	}

	engine.Publish = controller.InstallBinary
	engine.Warn = func(message string) { fmt.Fprintln(rootCommand.ErrOrStderr(), "Warning: "+message) }
	engine.Cleanup = func(ctx context.Context, purge bool, files func() error) error {
		return controller.Uninstall(ctx, purge, nil, files)
	}
	return engine, nil
}

func addDistribution(root *cobra.Command, build Build) {
	if build.Environment.Kind() != config.Production {
		for _, name := range []string{"upgrade", "uninstall"} {
			cmd := &cobra.Command{Use: name, Hidden: true, RunE: func(_ *cobra.Command, _ []string) error {
				target := "install"
				if name == "uninstall" {
					target = "uninstall"
				}
				return invalid("development builds use make " + target)
			}}
			if name == "uninstall" {
				cmd.Flags().Bool("purge", false, "Use make uninstall PURGE=1")
				cmd.Flags().Bool("yes", false, "Use make uninstall")
			}
			if name == "upgrade" {
				cmd.Aliases = []string{"update"}
			}
			root.AddCommand(cmd)
		}
		return
	}
	metadata := &cobra.Command{Use: "__release-metadata", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var err error
		if flag(cmd, "text") {
			_, err = fmt.Fprint(cmd.OutOrStdout(), distribution.Contract(build.Version).Text())
		} else {
			err = json.NewEncoder(cmd.OutOrStdout()).Encode(distribution.Contract(build.Version))
		}
		if err != nil {
			return failure("cannot write release metadata")
		}
		return nil
	}}
	metadata.Flags().Bool("text", false, "Bootstrap metadata")
	root.AddCommand(metadata)
	install := &cobra.Command{Use: "__install", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := distributionEngine(cmd.Context(), build, root)
		if err != nil {
			return distributionError(err)
		}
		e.NoShell = flag(cmd, "no-shell")
		e.Shell, err = distribution.LoginShell(cmd.Context())
		if err != nil {
			e.Warn("cannot determine login shell; configure PATH manually")
		}
		if err = e.Install(cmd.Context()); err != nil {
			return distributionError(err)
		}
		if e.Changed {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed Data Mate %s at %s\nProduction service remains stopped. Run data-mate mcp start when ready.\n", build.Version, config.ExecutablePath(e.Root))
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Data Mate %s at %s is already current.\n", build.Version, config.ExecutablePath(e.Root))
		}
		if err != nil {
			return failure("cannot write installation result")
		}
		if first, lookupErr := exec.LookPath("data-mate"); lookupErr == nil {
			resolved, pathErr := filepath.EvalSymlinks(first)
			if pathErr == nil && resolved != config.ExecutablePath(e.Root) {
				if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "Current PATH resolves another data-mate at %q; activate the loader below or open a new terminal.\n", first); err != nil {
					return failure("cannot write PATH diagnostic")
				}
			}
		}
		if e.Shell == "bash" || e.Shell == "zsh" {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Activate now: source %s\n", distribution.Quote(filepath.Join(e.Root.Path, "shell", "loader."+e.Shell)))
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Add %s to PATH.\n", distribution.Quote(filepath.Join(e.Home, ".local", "bin")))
		}
		if err != nil {
			return failure("cannot write activation instructions")
		}
		return nil
	}}
	install.Flags().Bool("no-shell", false, "Leave shell startup files unchanged")
	root.AddCommand(install)
	upgrade := &cobra.Command{Use: "upgrade", Aliases: []string{"update"}, Short: "Install the latest stable release", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		// Verify the selected installation before any network request.
		e, err := distributionEngine(cmd.Context(), build, root)
		if err != nil {
			return distributionError(err)
		}
		if _, err = e.Preview(cmd.Context()); err != nil {
			return distributionError(err)
		}
		candidate, err := distribution.NewClient().Latest(cmd.Context(), build.Version)
		if err != nil {
			return distributionError(err)
		}
		defer candidate.Close()
		return runDistributionChild(cmd, candidate.Path, "__install")
	}}
	root.AddCommand(upgrade)
	uninstall := &cobra.Command{Use: "uninstall", Short: "Remove the production installation", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := distributionEngine(cmd.Context(), build, root)
		if err != nil {
			return distributionError(err)
		}
		targets, err := e.Preview(cmd.Context())
		if err != nil {
			return distributionError(err)
		}
		if !flag(cmd, "yes") && !hasTerminal(cmd) {
			return invalid("noninteractive uninstall requires --yes")
		}
		if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "Remove owned production artifacts:\n%s\nPurge saved connections and credentials: %t\n", strings.Join(targets, "\n"), flag(cmd, "purge")); err != nil {
			return failure("cannot write uninstall preview")
		}
		if !flag(cmd, "yes") {
			f, err := openForm(cmd)
			if err != nil {
				return err
			}
			err = f.confirm()
			restore := f.restore()
			if err != nil {
				return err
			}
			if restore != nil {
				return failure("cannot restore terminal")
			}
		}
		return cleanupDistribution(cmd, e, flag(cmd, "purge"))
	}}
	uninstall.Flags().Bool("yes", false, "Confirm production cleanup")
	uninstall.Flags().Bool("purge", false, "Also remove saved connections and credentials")
	root.AddCommand(uninstall)
	for _, name := range []string{"__uninstall", "__cleanup"} {
		cmd := &cobra.Command{Use: name, Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := distributionEngine(cmd.Context(), build, root)
			if err != nil {
				return distributionError(err)
			}
			return cleanupDistribution(cmd, e, flag(cmd, "purge"))
		}}
		cmd.Flags().Bool("purge", false, "Remove saved connections and credentials")
		root.AddCommand(cmd)
	}
}

func cleanupDistribution(cmd *cobra.Command, e *distribution.Engine, purge bool) error {
	if err := e.Uninstall(cmd.Context(), purge); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		retry := "Rerun the installer, then retry uninstall."
		if _, statErr := os.Stat(config.ExecutablePath(e.Root)); statErr == nil {
			retry = "Retry: " + distribution.Quote(config.ExecutablePath(e.Root)) + " uninstall --yes"
			if purge {
				retry += " --purge"
			}
		} else if purge {
			retry = "Rerun the installer with --uninstall --purge."
		}
		return failure(distributionError(err).Error() + "\n" + retry)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), "Production cleanup complete.")
	return err
}
func runDistributionChild(cmd *cobra.Command, path string, args ...string) error {
	child := exec.CommandContext(cmd.Context(), path, args...)
	child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := child.Run(); err != nil {
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		return distributionError(err)
	}
	return nil
}
