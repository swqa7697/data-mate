package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/vault"
)

func newDB(override *string, keys vault.KeyProvider) *cobra.Command {
	db := &cobra.Command{Use: "db", Short: "Manage saved database connections", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, action := range []string{"add", "edit", "remove", "list"} {
		cmd := &cobra.Command{Use: action, Short: map[string]string{"add": "Save a connection", "edit": "Edit a connection", "remove": "Remove a connection and its credentials", "list": "List nonsecret connections"}[action], Args: cobra.MaximumNArgs(1)}
		if action == "add" || action == "list" {
			cmd.Args = cobra.NoArgs
		}
		if action == "remove" {
			cmd.Aliases = []string{"rm"}
		}
		if action == "list" {
			cmd.Aliases = []string{"ls"}
			cmd.Flags().Bool("json", false, "Versioned JSON output")
		} else {
			cmd.Flags().Bool("yes", false, "Confirm a complete operation")
		}
		if action == "add" || action == "edit" {
			profileFlags(cmd)
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error { return runDB(cmd, args, action, *override, keys) }
		db.AddCommand(cmd)
	}
	return db
}

func runDB(cmd *cobra.Command, args []string, action, override string, keys vault.KeyProvider) (result error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
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
	profiles, revision, err := config.Preview(ctx, root)
	if err != nil {
		return storageError(err, true)
	}
	slices.SortFunc(profiles.Connections, func(a, b config.Profile) int { return strings.Compare(a.Alias, b.Alias) })
	if action == "list" {
		return listProfiles(cmd, profiles)
	}
	stdinMode := flag(cmd, "password-stdin") || flag(cmd, "credentials-stdin")
	if stdinMode && !flag(cmd, "yes") {
		return invalid("stdin credentials require complete flags and --yes")
	}
	var ui *form
	defer func() {
		if ui != nil {
			if err := ui.restore(); err != nil && result == nil {
				result = failure("cannot restore terminal")
			}
		}
	}()
	getForm := func() (*form, error) {
		if stdinMode {
			return nil, invalid("stdin credentials cannot share input with forms")
		}
		if ui == nil {
			var err error
			ui, err = openForm(cmd)
			if err != nil {
				return nil, err
			}
		}
		return ui, nil
	}
	index := -1
	if action != "add" {
		alias := ""
		if len(args) > 0 {
			alias = args[0]
		} else {
			if len(profiles.Connections) == 0 {
				return invalid("no saved connections")
			}
			f, err := getForm()
			if err != nil {
				return err
			}
			for i, p := range profiles.Connections {
				if _, err = fmt.Fprintf(f.terminal, "%d. %s\n", i+1, p.Alias); err != nil {
					return failure("cannot write selection")
				}
			}
			choice, err := f.ask("Connection number or alias", "", false)
			if err != nil {
				return err
			}
			if n, e := strconv.Atoi(choice); e == nil && n > 0 && n <= len(profiles.Connections) {
				alias = profiles.Connections[n-1].Alias
			} else {
				alias = choice
			}
		}
		for i, p := range profiles.Connections {
			if p.Alias == alias {
				index = i
				break
			}
		}
		if index < 0 {
			return invalid("connection alias not found")
		}
	}
	var profile, original config.Profile
	patch := secretPatch{}
	if action == "add" {
		id, err := config.NewID()
		if err != nil {
			return failure("cannot generate connection identity")
		}
		profile = config.Profile{ID: id, Driver: "postgres", Connection: config.Connection{Port: 5432}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"}}
	} else {
		profile = profiles.Connections[index]
		original = profile
	}
	if action != "remove" {
		profile, patch, err = collectProfile(cmd, action, profile, original, getForm)
		if err != nil {
			return err
		}
		if action == "add" {
			profiles.Connections = append(profiles.Connections, profile)
		} else {
			profiles.Connections[index] = profile
		}
	} else {
		profiles.Connections = append(profiles.Connections[:index:index], profiles.Connections[index+1:]...)
	}
	b, err := json.Marshal(profiles)
	if err != nil {
		return invalid("invalid profile")
	}
	profiles, _, err = config.DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		return invalid("invalid profile settings or duplicate alias; check db help")
	}
	// Review goes to stderr; terminal output passes through the raw-mode renderer.
	var review io.Writer = cmd.ErrOrStderr()
	if ui != nil {
		review = ui.terminal
	}
	if err = previewProfile(review, action, profile, len(patch) > 0, hasTerminal(cmd)); err != nil {
		return err
	}
	if !flag(cmd, "yes") {
		f, err := getForm()
		if err != nil {
			return err
		}
		if err = f.confirm(); err != nil {
			return err
		}
	}
	if ui != nil {
		if err = ui.restore(); err != nil {
			return failure("cannot restore terminal")
		}
		ui = nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	store, err := config.Open(ctx, root, nil)
	if err != nil {
		return storageError(err, false)
	}
	defer store.Close()
	if keys == nil {
		keys = vault.Keychain{Interactive: hasTerminal(cmd)}
	}
	repo := vault.New(store, keys)
	mutation := vault.Mutation{Expected: revision, Profiles: profiles}
	if action == "edit" || len(patch) > 0 {
		var secrets vault.Secrets
		if original.CredentialRef != "" {
			lease, e := store.ReadLease(ctx)
			if e != nil {
				return storageError(e, false)
			}
			_, current, e := lease.ProfileSnapshot()
			if e == nil && current != revision {
				e = config.ErrRevision
			}
			if e == nil {
				secrets, e = repo.Credential(ctx, lease, original.ID)
			}
			lease.Release()
			if errors.Is(e, vault.ErrCredentialMissing) && repairComplete(profile, patch) {
				e = nil
			}
			if e != nil {
				return storageError(e, false)
			}
		}
		patch.apply(&secrets)
		if len(patch) > 0 {
			if secrets == (vault.Secrets{}) {
				for i := range mutation.Profiles.Connections {
					if mutation.Profiles.Connections[i].ID == profile.ID {
						mutation.Profiles.Connections[i].CredentialRef = ""
					}
				}
			} else {
				mutation.Replacements = map[string]vault.Secrets{profile.ID: secrets}
			}
		}
	}
	outcome, err := repo.Apply(ctx, mutation)
	if err != nil {
		if outcome.ProfilesSaved {
			return failure("profile change saved; credential cleanup failed and remains pending; the next confirmed add/edit/remove will retry cleanup")
		}
		if outcome.PublicationUncertain {
			return failure("profile publication could not be confirmed; run db list before retrying")
		}
		return storageError(err, false)
	}
	if _, err = fmt.Fprintln(cmd.OutOrStdout(), "Connection "+action+" completed."); err != nil {
		return failure("change saved; cannot write output")
	}
	return nil
}

func repairComplete(p config.Profile, patch secretPatch) bool {
	if _, ok := patch["password"]; !ok {
		return false
	}
	if p.Transport.SSH != nil {
		key := "ssh_password"
		if p.Transport.SSH.Auth == "key" {
			key = "ssh_private_key"
		}
		if _, ok := patch[key]; !ok {
			return false
		}
	}
	if p.Transport.Proxy != nil && p.Transport.Proxy.Username != "" {
		if _, ok := patch["proxy_password"]; !ok {
			return false
		}
	}
	return true
}
func storageError(err error, input bool) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, config.ErrRevision) {
		return failure("profiles changed since preview; run the command again")
	}
	if errors.Is(err, vault.ErrCredentialMissing) {
		return failure(string(contracts.CredentialMissing) + ": " + vault.ErrCredentialMissing.Error())
	}
	for _, safe := range []error{vault.ErrMissing, vault.ErrDenied, vault.ErrLocked, vault.ErrUnavailable, vault.ErrRepair, vault.ErrLimit, vault.ErrBinding, config.ErrOwnership, config.ErrStale, config.ErrPurging} {
		if errors.Is(err, safe) {
			return failure(safe.Error())
		}
	}
	if input {
		return invalid("invalid or missing profile configuration; existing files were preserved")
	}
	return failure("cannot save connection state; existing credentials were preserved")
}
func scopeSummary(s config.Scope) string {
	if s.Mode == "all" {
		return "all accessible tables"
	}
	if len(s.Schemas) == 0 && len(s.Tables) == 0 {
		return "none"
	}
	b, _ := json.Marshal(s)
	return string(b)
}
func previewProfile(w io.Writer, action string, p config.Profile, secretsChanged, color bool) error {
	title := "Review " + action
	if _, disabled := os.LookupEnv("NO_COLOR"); color && !disabled {
		title = "\x1b[36m" + title + "\x1b[0m"
	}
	secretState := "unchanged"
	if secretsChanged {
		secretState = "changing (hidden)"
	}
	transport, _ := json.Marshal(p.Transport)
	limits, _ := json.Marshal(p.Limits)
	_, err := fmt.Fprintf(w, "%s\nAlias: %s\nDriver: %s\nHost: %q\nPort: %d\nDatabase: %q\nUsername: %q\nTransport: %s\nScope: %s\nLimits: %s\nCredentials: %s\n", title, p.Alias, p.Driver, p.Connection.Host, p.Connection.Port, p.Connection.Database, p.Connection.Username, transport, scopeSummary(p.Scope), limits, secretState)
	if err != nil {
		return failure("cannot write preview")
	}
	if p.Transport.SSH != nil || p.Transport.Proxy != nil {
		if _, err = fmt.Fprintln(w, "Transport settings will be saved; runtime support is not available yet."); err != nil {
			return failure("cannot write preview")
		}
	}
	return nil
}
func listProfiles(cmd *cobra.Command, p config.Profiles) error {
	type entry struct {
		Alias      string            `json:"alias"`
		Driver     string            `json:"driver"`
		Connection config.Connection `json:"connection"`
		Scope      config.Scope      `json:"scope"`
	}
	entries := make([]entry, 0, len(p.Connections))
	for _, p := range p.Connections {
		entries = append(entries, entry{p.Alias, p.Driver, p.Connection, p.Scope})
	}
	if flag(cmd, "json") {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Version     int     `json:"version"`
			Connections []entry `json:"connections"`
		}{1, entries}); err != nil {
			return failure("cannot write output")
		}
		return nil
	}
	if len(entries) == 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No saved connections."); err != nil {
			return failure("cannot write output")
		}
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %q:%d  database=%q  scope=%s\n", e.Alias, e.Driver, e.Connection.Host, e.Connection.Port, e.Connection.Database, scopeSummary(e.Scope)); err != nil {
			return failure("cannot write output")
		}
	}
	return nil
}
