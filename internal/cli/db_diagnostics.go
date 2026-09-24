package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/postgres"
	"github.com/swqa7697/data-mate/internal/transport"
	"github.com/swqa7697/data-mate/internal/vault"
)

// This seam owns only the two CLI database operations; production always uses
// the same shared PostgreSQL driver as query/catalog callers.
type cliDatabase interface {
	ValidateProfile(config.Profile) error
	Test(context.Context, database.Access) (database.Readiness, error)
	BrowseScope(context.Context, database.Access, database.ScopeRequest) (database.ScopePage, error)
	Close()
}
type databaseFactory func() (cliDatabase, error)

func defaultDatabase() (cliDatabase, error) { return postgres.New() }

func databaseError(err error) error {
	var e *database.Error
	if errors.As(err, &e) {
		if e.Code == contracts.Cancelled {
			return context.Canceled
		}
		if e.Code == contracts.ConfigInvalid || e.Code == contracts.InvalidArgument {
			return invalid(e.Error())
		}
		return failure(e.Error())
	}
	return failure("database operation failed")
}
func accessUnderLease(ctx context.Context, l *config.Lease, repo *vault.Repository, p config.Profile) (database.Access, error) {
	s, err := repo.Credential(ctx, l, p.ID)
	if err != nil {
		return database.Access{}, err
	}
	var hosts []byte
	if p.Transport.SSH != nil {
		hosts, err = transport.ReadKnownHosts(l)
		if err != nil {
			return database.Access{}, database.Fail(contracts.ConnectFailed, "cannot load owned SSH host pins; verify known_hosts", false)
		}
	}
	return database.NewAccess(p, s.Password).WithTransport(transport.Credentials{SSHPassword: s.SSHPassword, SSHPrivateKey: s.SSHPrivateKey, SSHKeyPassphrase: s.SSHKeyPassphrase, ProxyPassword: s.ProxyPassword}, hosts), nil
}

type diagnosticResult struct {
	Alias  string             `json:"alias"`
	OK     bool               `json:"ok"`
	Stage  string             `json:"stage"`
	Stages []database.Stage   `json:"stages"`
	Error  *contracts.Failure `json:"error,omitempty"`
}

func (r *diagnosticResult) fail(stage string, code contracts.Code, message string, retry bool) {
	r.Stage = stage
	r.OK = false
	r.Error = &contracts.Failure{Code: code, Message: message, Retryable: retry}
	r.Stages = append(r.Stages, database.Stage{Stage: stage, Error: r.Error})
}
func newDBTest(override *string, keys vault.KeyProvider, factory databaseFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "test [alias]", Short: "Check staged connection and read-only transaction readiness", Args: cobra.MaximumNArgs(1)}
	cmd.Flags().Bool("json", false, "Versioned JSON results with reached diagnostic stages")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return runDBTest(cmd, args, *override, keys, factory) }
	return cmd
}
func runDBTest(cmd *cobra.Command, args []string, override string, keys vault.KeyProvider, factory databaseFactory) error {
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
		store, err := config.Open(cmd.Context(), root, nil)
		if err != nil {
			return storageError(err, true)
		}
		defer store.Close()
		if keys == nil {
			keys = vault.Keychain{Interactive: hasTerminal(cmd)}
		}
		repo := vault.New(store, keys)
		d, err := factory()
		if err != nil {
			return failure("cannot initialize database driver")
		}
		defer d.Close()
		for _, p := range selected {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			results = append(results, testProfile(cmd.Context(), store, repo, d, p))
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
func testProfile(parent context.Context, store *config.Store, repo *vault.Repository, d cliDatabase, want config.Profile) diagnosticResult {
	r := diagnosticResult{Alias: want.Alias, Stage: "config", Stages: []database.Stage{}}
	timeout := config.DefaultLimits().QueryTimeoutMS
	if want.Limits != nil {
		timeout = want.Limits.QueryTimeoutMS
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	l, err := store.ReadLease(ctx)
	if err != nil {
		code := contracts.ConfigInvalid
		if errors.Is(err, context.Canceled) {
			code = contracts.Cancelled
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = contracts.QueryTimeout
		}
		r.fail("config", code, "cannot acquire current profile snapshot", true)
		return r
	}
	defer l.Release()
	profiles, _, err := l.ProfileSnapshot()
	if err != nil {
		r.fail("config", contracts.ConfigInvalid, "invalid or missing profile configuration", false)
		return r
	}
	var p *config.Profile
	for i := range profiles.Connections {
		if profiles.Connections[i].ID == want.ID && profiles.Connections[i].Alias == want.Alias {
			p = &profiles.Connections[i]
			break
		}
	}
	if p == nil {
		r.fail("config", contracts.ConnectionNotFound, "connection changed; run the command again", true)
		return r
	}
	if err = d.ValidateProfile(*p); err != nil {
		r.fail("config", contracts.ConfigInvalid, "invalid driver profile settings; repair with db edit", false)
		return r
	}
	r.Stages = append(r.Stages, database.Stage{Stage: "config", OK: true})
	a, err := accessUnderLease(ctx, l, repo, *p)
	if err != nil {
		var safe *database.Error
		if errors.As(err, &safe) {
			r.Stages = append(r.Stages, database.Stage{Stage: "vault", OK: true})
			r.fail("dial", safe.Code, safe.Message, safe.Retryable)
			return r
		}
		code := contracts.VaultUnavailable
		message := "cannot load credentials; unlock the vault or repair credentials with db edit"
		if errors.Is(err, vault.ErrCredentialMissing) {
			code = contracts.CredentialMissing
			message = "credential bundle missing; repair with db edit"
		}
		if ctx.Err() != nil {
			code = contracts.QueryTimeout
			message = "credential access timed out"
			if parent.Err() != nil {
				code = contracts.Cancelled
				message = "connection test canceled"
			}
		}
		r.fail("vault", code, message, true)
		return r
	}
	r.Stages = append(r.Stages, database.Stage{Stage: "vault", OK: true})
	ready, err := d.Test(ctx, a)
	for _, s := range ready.Stages {
		if s.Stage != "config" || !s.OK {
			r.Stages = append(r.Stages, s)
		}
	}
	r.Stage = ready.Stage
	if err != nil {
		var e *database.Error
		if errors.As(err, &e) {
			r.Error = &e.Failure
		} else {
			r.Error = &contracts.Failure{Code: contracts.ConnectFailed, Message: "connection test failed", Retryable: true}
		}
		if r.Stage == "" {
			r.Stage = "dial"
			r.Stages = append(r.Stages, database.Stage{Stage: r.Stage, Error: r.Error})
		}
		return r
	}
	r.OK = true
	return r
}
