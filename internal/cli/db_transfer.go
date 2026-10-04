package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/service"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/sys/unix"
)

// secretLabels name prompts for credential keys; ssh_private_key is a path prompt.
var secretLabels = map[string]string{"password": "Password", "ssh_password": "SSH password", "ssh_private_key": "SSH key file", "proxy_password": "Proxy password"}

// importItem is a decoded row with its resolved action: add, update, skip or unchanged.
type importItem struct {
	importRow
	action   string
	required []string
	pin      *service.HostPin
}

func (it importItem) submitted() bool { return it.action == "add" || it.action == "update" }

func newDBImport(override *string, factory managementFactory, build Build) *cobra.Command {
	cmd := &cobra.Command{Use: "import <file.csv>", Short: "Add or update connections from a CSV file", Args: cobra.ExactArgs(1)}
	cmd.Flags().Bool("yes", false, "Confirm a complete import")
	cmd.Flags().String("on-conflict", "", "Existing alias policy: stop, skip, or update")
	cmd.Flags().Bool("ssh-enroll", false, "Interactively verify and save SSH host fingerprints")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runDBImport(cmd, args[0], *override, factory, build)
	}
	return cmd
}

func newDBExport(override *string, factory managementFactory, build Build) *cobra.Command {
	cmd := &cobra.Command{Use: "export <file.csv>", Short: "Write connection settings to a CSV file without secrets", Args: cobra.ExactArgs(1)}
	cmd.Flags().Bool("yes", false, "Replace an existing file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runDBExport(cmd, args[0], *override, factory, build)
	}
	return cmd
}

func runDBImport(cmd *cobra.Command, path, override string, factory managementFactory, build Build) (result error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	policy := str(cmd, "on-conflict")
	if policy != "" && !slices.Contains([]string{"stop", "skip", "update"}, policy) {
		return invalid("--on-conflict must be stop, skip, or update")
	}
	terminal := hasTerminal(cmd)
	if flag(cmd, "ssh-enroll") && (flag(cmd, "yes") || !terminal) {
		return invalid("SSH enrollment requires interactive fingerprint and final save confirmations; omit --yes")
	}
	if !flag(cmd, "yes") && !terminal {
		return invalid("complete flags and --yes are required without a terminal")
	}
	exe, err := os.Executable()
	if err != nil {
		return failure("cannot locate executable")
	}
	root, err := build.resolveRoot(override, exe)
	if err != nil {
		return invalid("invalid installation root")
	}
	profiles, revision, err := config.Preview(ctx, root)
	if err != nil {
		return storageError(err, true)
	}
	data, err := readImportFile(path)
	if err != nil {
		return err
	}
	rows, err := decodeImport(ctx, data)
	clear(data)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), "No connections to import."); err != nil {
			return failure("cannot write output")
		}
		return nil
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
		if ui == nil {
			var err error
			if ui, err = openForm(cmd); err != nil {
				return nil, err
			}
		}
		return ui, nil
	}
	if conflicts := existingAliases(rows, profiles); len(conflicts) > 0 {
		listed := strings.Join(conflicts, ", ")
		switch {
		case policy == "stop":
			return invalid("existing connections: " + listed)
		case policy == "" && !terminal:
			return invalid("existing connections: " + listed + "; pass --on-conflict stop, skip, or update")
		case policy == "":
			f, err := getForm()
			if err != nil {
				return err
			}
			if policy, err = askConflictPolicy(f, listed); err != nil {
				return err
			}
		}
	}
	items, err := planImport(rows, profiles, policy)
	if err != nil {
		return err
	}
	if err = completeImportSecrets(cmd, items, getForm); err != nil {
		return err
	}
	if flag(cmd, "ssh-enroll") {
		if err = enrollImportHosts(ctx, root, items, getForm); err != nil {
			return err
		}
	}
	if slices.ContainsFunc(items, importItem.submitted) {
		// Review goes to stderr; terminal output passes through the raw-mode renderer.
		var review io.Writer = cmd.ErrOrStderr()
		if ui != nil {
			review = ui.terminal
		}
		if err = reviewImport(review, items, terminal); err != nil {
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
	}
	if ui != nil {
		err = ui.restore()
		ui = nil
		if err != nil {
			return failure("cannot restore terminal")
		}
	}
	return submitImport(cmd, root, factory, items, revision)
}

// readImportFile reads one bounded regular file. Import does not police the
// caller's CSV permissions; it only refuses devices, FIFOs and oversized input.
func readImportFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, invalid("cannot read CSV file")
	}
	f := os.NewFile(uintptr(fd), "connection CSV")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxImportBytes {
		return nil, invalid("CSV must be a regular file of at most 8 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxImportBytes+1))
	if err != nil || len(b) > maxImportBytes {
		clear(b)
		return nil, invalid("CSV must be a regular file of at most 8 MiB")
	}
	return b, nil
}

// existingAliases lists CSV aliases that are already saved, in file order.
func existingAliases(rows []importRow, current config.Profiles) []string {
	var conflicts []string
	for _, row := range rows {
		if slices.ContainsFunc(current.Connections, func(p config.Profile) bool { return p.Alias == row.profile.Alias }) {
			conflicts = append(conflicts, row.profile.Alias)
		}
	}
	return conflicts
}

// askConflictPolicy requires an explicit choice; there is no default answer.
func askConflictPolicy(f *form, listed string) (string, error) {
	if _, err := fmt.Fprintf(f.terminal, "Existing connections: %s\n", listed); err != nil {
		return "", failure("cannot write prompt")
	}
	for {
		answer, err := f.ask("Stop, skip, or update? [stop/skip/update]", "", false)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(answer) {
		case "stop":
			return "", context.Canceled
		case "skip":
			return "skip", nil
		case "update":
			return "update", nil
		}
	}
}

// planImport resolves each row against saved connections under a conflict
// policy, assigns identities and derives required and cleared credentials.
func planImport(rows []importRow, current config.Profiles, policy string) ([]importItem, error) {
	saved := make(map[string]config.Profile, len(current.Connections))
	for _, p := range current.Connections {
		saved[p.Alias] = p
	}
	projected := slices.Clone(current.Connections)
	items := make([]importItem, 0, len(rows))
	for _, row := range rows {
		item := importItem{importRow: row}
		item.secrets = maps.Clone(row.secrets)
		original, exists := saved[row.profile.Alias]
		switch {
		case exists && policy == "skip":
			item.action = "skip"
		case exists && policy == "update":
			item.profile.ID, item.profile.CredentialRef = original.ID, original.CredentialRef
			item.action = "update"
			if reflect.DeepEqual(item.profile, original) && len(item.secrets) == 0 && item.keyFile == "" {
				item.action = "unchanged"
			}
		case exists:
			return nil, invalid("existing connections require --on-conflict skip or update")
		default:
			id, err := config.NewID()
			if err != nil {
				return nil, failure("cannot generate connection identity")
			}
			item.profile.ID = id
			item.action = "add"
		}
		if !item.submitted() {
			items = append(items, item)
			continue
		}
		item.required = credentialTransition(item.profile, original, item.action == "add", item.secrets)
		if item.keyFile != "" {
			item.required = slices.DeleteFunc(item.required, func(k string) bool { return k == "ssh_private_key" })
		}
		if item.action == "add" {
			projected = append(projected, item.profile)
		} else {
			projected[slices.IndexFunc(projected, func(p config.Profile) bool { return p.ID == original.ID })] = item.profile
		}
		items = append(items, item)
	}
	if len(projected) > maxConnections {
		return nil, invalid("import would exceed 128 saved connections")
	}
	b, err := json.Marshal(config.Profiles{Version: 1, Connections: projected})
	if err != nil {
		return nil, invalid("invalid connection settings")
	}
	if _, _, err = config.DecodeProfiles(bytes.NewReader(b)); err != nil {
		return nil, invalid("imported connections conflict with saved settings")
	}
	return items, nil
}

// credentialTransition returns the secrets a candidate newly needs, as db
// add/edit prompt for them, and adds clears for secrets its transport stopped
// using. A new connection needs every secret vault.Patch.Complete requires.
func credentialTransition(p, original config.Profile, added bool, patch secretPatch) []string {
	var required []string
	if added {
		required = append(required, "password")
	}
	before, after := original.Transport.SSH, p.Transport.SSH
	if after != nil && after.Auth == "password" && (before == nil || before.Auth != "password") {
		required = append(required, "ssh_password")
	}
	if after != nil && after.Auth == "key" && (before == nil || before.Auth != "key") {
		required = append(required, "ssh_private_key")
	}
	if proxy := p.Transport.Proxy; proxy != nil && proxy.Username != "" && (original.Transport.Proxy == nil || original.Transport.Proxy.Username == "") {
		required = append(required, "proxy_password")
	}
	clearUnset := func(keys ...string) {
		for _, k := range keys {
			if _, supplied := patch[k]; !supplied {
				patch[k] = ""
			}
		}
	}
	switch {
	case before != nil && after == nil:
		clearUnset("ssh_password", "ssh_private_key", "ssh_key_passphrase")
	case before != nil && before.Auth == "password" && after.Auth == "key":
		clearUnset("ssh_password")
	case before != nil && before.Auth == "key" && after.Auth == "password":
		clearUnset("ssh_private_key", "ssh_key_passphrase")
	}
	if before := original.Transport.Proxy; before != nil && before.Username != "" && (p.Transport.Proxy == nil || p.Transport.Proxy.Username == "") {
		clearUnset("proxy_password")
	}
	return required
}

// completeImportSecrets reads listed key files, then prompts in row order for
// each required secret the CSV left empty. Without a terminal, every required
// secret must be in the CSV; nothing is prompted.
func completeImportSecrets(cmd *cobra.Command, items []importItem, getForm func() (*form, error)) error {
	terminal := hasTerminal(cmd)
	for _, it := range items {
		for _, key := range it.required {
			if _, ok := it.secrets[key]; !ok && !terminal {
				column := key
				if key == "ssh_private_key" {
					column = "ssh_key_file"
				}
				return invalid(fmt.Sprintf("line %d: %s is required without a terminal", it.line, column))
			}
		}
	}
	for i := range items {
		it := &items[i]
		if !it.submitted() {
			continue
		}
		newKey := false
		if it.keyFile != "" {
			key, err := readKeyFile(it.keyFile)
			if err != nil {
				return invalid(fmt.Sprintf("line %d: cannot read ssh_key_file", it.line))
			}
			it.secrets["ssh_private_key"] = key
			newKey = true
		}
		for _, key := range it.required {
			if _, ok := it.secrets[key]; ok {
				continue
			}
			f, err := getForm()
			if err != nil {
				return err
			}
			if key == "ssh_private_key" {
				if it.secrets[key], err = askKeyFile(f, it.profile.Alias); err != nil {
					return err
				}
				newKey = true
				continue
			}
			if it.secrets[key], err = f.ask(secretLabels[key]+" for "+it.profile.Alias, "", true); err != nil {
				return err
			}
		}
		// A new key's passphrase is asked as db add does; otherwise it has none.
		if _, ok := it.secrets["ssh_key_passphrase"]; newKey && !ok {
			it.secrets["ssh_key_passphrase"] = ""
			if terminal && !flag(cmd, "yes") {
				f, err := getForm()
				if err != nil {
					return err
				}
				if it.secrets["ssh_key_passphrase"], err = f.ask("SSH key passphrase for "+it.profile.Alias+" (blank for none)", "", true); err != nil {
					return err
				}
			}
		}
		if err := validateCandidate(it.profile, it.secrets); err != nil {
			var public *Error
			if errors.As(err, &public) {
				return invalid(fmt.Sprintf("line %d: %s", it.line, public.Message))
			}
			return err
		}
	}
	return nil
}

// askKeyFile repeats until a readable key file is named or the user cancels.
func askKeyFile(f *form, alias string) (string, error) {
	for {
		path, err := f.ask("SSH key file for "+alias, "", false)
		if err != nil {
			return "", err
		}
		key, err := readKeyFile(path)
		if err == nil {
			return key, nil
		}
		if _, err = fmt.Fprintln(f.terminal, "Cannot read that SSH key file; enter a readable private key path."); err != nil {
			return "", failure("cannot write prompt")
		}
	}
}

// enrollImportHosts confirms each distinct SSH endpoint once and attaches the
// same pin to every submitted row that uses it.
func enrollImportHosts(ctx context.Context, root config.Root, items []importItem, getForm func() (*form, error)) error {
	pins := make(map[string]*service.HostPin)
	for i := range items {
		s := items[i].profile.Transport.SSH
		if s == nil || !items[i].submitted() {
			continue
		}
		address := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
		if pins[address] == nil {
			key, err := enrollHost(ctx, root, *s, getForm)
			if err != nil {
				return err
			}
			pins[address] = &service.HostPin{Address: key.Address, Key: key.Key.Marshal()}
		}
		items[i].pin = pins[address]
	}
	return nil
}

func reviewImport(w io.Writer, items []importItem, color bool) error {
	for _, it := range items {
		if it.submitted() {
			if err := previewProfile(w, it.action, it.profile, len(it.secrets) > 0, color); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", it.action, it.profile.Alias); err != nil {
			return failure("cannot write preview")
		}
	}
	return nil
}

// submitImport saves each row as its own mutation chained on the revision the
// previous row published. Rows that fail cleanly are reported and skipped;
// anything that leaves state uncertain or changed by others stops the import.
func submitImport(cmd *cobra.Command, root config.Root, factory managementFactory, items []importItem, revision config.Revision) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	stop := func(err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), "Import stopped; remaining connections were not attempted.")
		return err
	}
	var client managementClient
	defer func() {
		if client != nil {
			client.Close()
		}
	}()
	failed := false
	last := revision
	for _, it := range items {
		status := map[string]string{"add": "added", "update": "updated", "skip": "skipped", "unchanged": "unchanged"}[it.action]
		if it.submitted() {
			if err := ctx.Err(); err != nil {
				return stop(err)
			}
			profiles, current, err := config.Preview(ctx, root)
			if err != nil {
				return stop(storageError(err, false))
			}
			if current != last {
				return stop(failure("profiles changed during import; run db list before retrying"))
			}
			if index := slices.IndexFunc(profiles.Connections, func(p config.Profile) bool { return p.ID == it.profile.ID }); index >= 0 {
				profiles.Connections[index] = it.profile
			} else {
				profiles.Connections = append(profiles.Connections, it.profile)
			}
			if client == nil {
				if client, err = factory(ctx, root); err != nil {
					return stop(serviceError(err))
				}
			}
			mutation := vault.Mutation{Expected: last, Profiles: profiles}
			if len(it.secrets) > 0 {
				mutation.Patches = map[string]vault.Patch{it.profile.ID: vault.Patch(it.secrets)}
			}
			reply, err := client.Request(ctx, service.ManagementRequest{Operation: "mutate", ProfileID: it.profile.ID, Interactive: hasTerminal(cmd), Mutation: &mutation, Pin: it.pin})
			switch {
			case err == nil && reply.Outcome != nil:
				last = reply.Outcome.Revision
			case err != nil && rowFailed(reply, err):
				failed = true
				status = "FAIL  " + storageError(err, false).Error()
			default:
				return stop(mutationError(ctx, reply.Outcome, err))
			}
		}
		if _, err := fmt.Fprintf(out, "%s  %s\n", it.profile.Alias, status); err != nil {
			return stop(failure("cannot write output"))
		}
	}
	if failed {
		return failure("one or more connection imports failed")
	}
	return nil
}

// rowFailed reports a mutation that was cleanly rejected without publishing,
// so later rows can still run against the unchanged revision.
func rowFailed(reply service.ManagementReply, err error) bool {
	if reply.Outcome == nil || reply.Outcome.ProfilesSaved || reply.Outcome.PublicationUncertain {
		return false
	}
	var dbError *database.Error
	return (errors.As(err, &dbError) && dbError.Code != contracts.Cancelled) || errors.Is(err, vault.ErrCredentialMissing)
}

func runDBExport(cmd *cobra.Command, path, override string, factory managementFactory, build Build) (result error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
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
	profiles, revision, err := config.Preview(ctx, root)
	if err != nil {
		return storageError(err, true)
	}
	slices.SortFunc(profiles.Connections, func(a, b config.Profile) int { return strings.Compare(a.Alias, b.Alias) })
	// Settle replacement before any keyring prompt.
	replace := false
	if st, err := os.Lstat(path); err == nil {
		if st.IsDir() {
			return invalid("export path is a directory")
		}
		replace = true
		if !flag(cmd, "yes") {
			if !hasTerminal(cmd) {
				return invalid("export file exists; pass --yes to replace it")
			}
			f, err := openForm(cmd)
			if err != nil {
				return err
			}
			err = f.consent("Replace existing file? [y/N]")
			if e := f.restore(); e != nil && err == nil {
				err = failure("cannot restore terminal")
			}
			if err != nil {
				return err
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return invalid("cannot access export path")
	}
	presence := make(map[string]service.CredentialPresence)
	if slices.ContainsFunc(profiles.Connections, func(p config.Profile) bool { return p.CredentialRef != "" }) {
		client, err := factory(ctx, root)
		if err != nil {
			return serviceError(err)
		}
		reply, err := client.Request(ctx, service.ManagementRequest{Operation: "credential-presence", Expected: revision, Interactive: hasTerminal(cmd)})
		client.Close()
		if err != nil {
			return storageError(err, false)
		}
		for _, c := range reply.Credentials {
			presence[c.ProfileID] = c
		}
	}
	var content bytes.Buffer
	if err = encodeExport(&content, profiles.Connections, presence); err != nil {
		return failure("cannot encode connections")
	}
	if err = writeExport(path, content.Bytes(), replace); err != nil {
		return err
	}
	noun := "connections"
	if len(profiles.Connections) == 1 {
		noun = "connection"
	}
	if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Exported %d %s.\n", len(profiles.Connections), noun); err != nil {
		return failure("export written; cannot write output")
	}
	return nil
}

// writeExport publishes a complete owner-only file. A new path is created
// exclusively; replacement renames a synced sibling, so a symlink at the path
// is replaced rather than followed and readers never see a partial file.
func writeExport(path string, data []byte, replace bool) error {
	write := func(f *os.File) error {
		_, err := f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		if e := f.Close(); err == nil {
			err = e
		}
		return err
	}
	if !replace {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
		if errors.Is(err, fs.ErrExist) {
			return invalid("export file exists; pass --yes to replace it")
		}
		if err != nil {
			return invalid("cannot create export file")
		}
		if err = write(f); err != nil {
			_ = os.Remove(path)
			return failure("cannot write export file")
		}
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".data-mate-export-*")
	if err != nil {
		return invalid("cannot create export file")
	}
	if err = write(f); err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return failure("cannot write export file")
	}
	return nil
}
