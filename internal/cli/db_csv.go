package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/service"
)

const (
	// maxImportBytes matches the cap on all stored credential ciphertext.
	maxImportBytes = 8 << 20
	maxConnections = 128
	// noSecret marks a credential the connection explicitly does not use. An
	// empty credential cell instead means the value was not supplied.
	noSecret = "<none>"
	// placeholderID lets one row pass profile validation before it has an identity.
	placeholderID = "00000000-0000-4000-8000-000000000000"
)

var (
	// settingColumns map to the db add flag of the same name with "_" as "-".
	settingColumns  = []string{"alias", "driver", "host", "port", "database", "username", "tls", "tls_ca", "ssh_host", "ssh_port", "ssh_user", "proxy", "proxy_user", "query_timeout", "max_rows", "max_result_bytes"}
	integerColumns  = []string{"port", "ssh_port", "max_rows", "max_result_bytes"}
	requiredColumns = []string{"alias", "host", "database", "username"}
	secretColumns   = []string{"password", "ssh_password", "ssh_key_passphrase", "proxy_password"}
	// csvColumns is the export order and the complete import vocabulary.
	csvColumns = []string{"alias", "driver", "host", "port", "database", "username", "tls", "tls_ca", "ssh_host", "ssh_port", "ssh_user", "ssh_auth", "proxy", "proxy_user", "query_timeout", "max_rows", "max_result_bytes", "password", "ssh_key_file", "ssh_password", "ssh_key_passphrase", "proxy_password"}
)

// importRow is one validated CSV connection. Secrets holds only supplied
// credential cells, with noSecret stored as an explicit empty value.
type importRow struct {
	line    int
	profile config.Profile
	secrets secretPatch
	keyFile string
}

// decodeImport validates a whole connection CSV without reading key files or
// state. Errors name lines and fixed column names, never cell values.
func decodeImport(ctx context.Context, data []byte) ([]importRow, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, invalid("CSV must be UTF-8 text")
	}
	r := csv.NewReader(bytes.NewReader(data))
	header, err := r.Read()
	if err != nil {
		return nil, invalid("CSV header is missing or malformed")
	}
	index := make(map[string]int, len(header))
	for i, name := range header {
		if !slices.Contains(csvColumns, name) {
			return nil, invalid(fmt.Sprintf("CSV column %d is unknown", i+1))
		}
		if _, duplicate := index[name]; duplicate {
			return nil, invalid(fmt.Sprintf("CSV column %d is duplicated", i+1))
		}
		index[name] = i
	}
	for _, name := range requiredColumns {
		if _, ok := index[name]; !ok {
			return nil, invalid("CSV requires alias, host, database, and username columns")
		}
	}
	var rows []importRow
	aliases := make(map[string]bool)
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			var parse *csv.ParseError
			if errors.As(err, &parse) {
				return nil, invalid(fmt.Sprintf("line %d: malformed CSV record", parse.StartLine))
			}
			return nil, invalid("malformed CSV")
		}
		if len(rows) == maxConnections {
			return nil, invalid("CSV exceeds 128 connections")
		}
		line, _ := r.FieldPos(0)
		row, err := decodeRow(ctx, line, func(name string) string {
			if i, ok := index[name]; ok {
				return record[i]
			}
			return ""
		})
		if err != nil {
			return nil, err
		}
		if aliases[row.profile.Alias] {
			return nil, invalid(fmt.Sprintf("line %d: alias repeats an earlier row", line))
		}
		aliases[row.profile.Alias] = true
		rows = append(rows, row)
	}
}

// decodeRow applies cells through the db add flag set so a CSV value means
// exactly what the matching flag means.
func decodeRow(ctx context.Context, line int, cell func(string) string) (importRow, error) {
	fail := func(message string) (importRow, error) {
		return importRow{}, invalid(fmt.Sprintf("line %d: %s", line, message))
	}
	for _, name := range requiredColumns {
		if cell(name) == "" {
			return fail(name + " is required")
		}
	}
	options := &cobra.Command{}
	options.SetContext(ctx)
	profileFlags(options)
	for _, name := range settingColumns {
		value := cell(name)
		if value == "" {
			continue
		}
		// pflag parses integers in base 0; CSV integers are decimal only.
		if slices.Contains(integerColumns, name) {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fail(name + " must be a decimal integer")
			}
			value = strconv.Itoa(n)
		}
		if options.Flags().Set(strings.ReplaceAll(name, "_", "-"), value) != nil {
			return fail("invalid " + name)
		}
	}
	row := importRow{line: line, profile: defaultProfile(placeholderID), secrets: secretPatch{}, keyFile: cell("ssh_key_file")}
	if err := applyOptions(options, &row.profile); err != nil {
		var public *Error
		if errors.As(err, &public) {
			return fail(public.Message)
		}
		return fail("invalid connection settings")
	}
	for _, name := range secretColumns {
		switch value := cell(name); value {
		case "":
		case noSecret:
			row.secrets[name] = ""
		default:
			row.secrets[name] = value
		}
	}
	ssh := row.profile.Transport.SSH
	auth := cell("ssh_auth")
	if row.keyFile != "" && auth == "" {
		auth = "key"
	}
	switch {
	case auth != "" && auth != "password" && auth != "key":
		return fail("ssh_auth must be password or key")
	case auth != "" && ssh == nil:
		return fail("ssh_auth and ssh_key_file require SSH settings")
	case auth != "":
		ssh.Auth = auth
	}
	if row.keyFile != "" && (row.keyFile == noSecret || !filepath.IsAbs(row.keyFile)) {
		return fail("ssh_key_file must be an absolute path")
	}
	if ssh != nil && ssh.Auth == "key" && row.secrets["ssh_password"] != "" {
		return fail("ssh_password requires ssh_auth=password")
	}
	if ssh != nil && ssh.Auth == "password" && (row.keyFile != "" || row.secrets["ssh_key_passphrase"] != "") {
		return fail("ssh_key_file and ssh_key_passphrase require ssh_auth=key")
	}
	if err := validateCandidate(row.profile, row.secrets); err != nil {
		var public *Error
		if errors.As(err, &public) {
			return fail(public.Message)
		}
		return fail("invalid connection settings")
	}
	b, err := json.Marshal(config.Profiles{Version: 1, Connections: []config.Profile{row.profile}})
	if err != nil {
		return fail("invalid connection settings")
	}
	if _, _, err = config.DecodeProfiles(bytes.NewReader(b)); err != nil {
		return fail("invalid connection settings")
	}
	return row, nil
}

// encodeExport writes nonsecret settings and credential cells that are either
// empty or noSecret. A credentialed profile absent from presence is unknown.
func encodeExport(w io.Writer, profiles []config.Profile, presence map[string]service.CredentialPresence) error {
	out := csv.NewWriter(w)
	if err := out.Write(csvColumns); err != nil {
		return err
	}
	for _, p := range profiles {
		limits := config.DefaultLimits()
		if p.Limits != nil {
			limits = *p.Limits
		}
		values := map[string]string{
			"alias": p.Alias, "driver": p.Driver, "host": p.Connection.Host, "port": strconv.Itoa(p.Connection.Port),
			"database": p.Connection.Database, "username": p.Connection.Username,
			"tls": strconv.FormatBool(p.Transport.TLS.Mode == "verify-full"), "tls_ca": p.Transport.TLS.CAFile,
			"query_timeout": strconv.Itoa(limits.QueryTimeoutMS) + "ms", "max_rows": strconv.Itoa(limits.MaxRows), "max_result_bytes": strconv.Itoa(limits.MaxResultBytes),
		}
		if s := p.Transport.SSH; s != nil {
			values["ssh_host"], values["ssh_port"], values["ssh_user"], values["ssh_auth"] = s.Host, strconv.Itoa(s.Port), s.User, s.Auth
		}
		if x := p.Transport.Proxy; x != nil {
			values["proxy"], values["proxy_user"] = "socks5://"+net.JoinHostPort(x.Host, strconv.Itoa(x.Port)), x.Username
		}
		for _, name := range unsetSecrets(p, presence) {
			values[name] = noSecret
		}
		record := make([]string, len(csvColumns))
		for i, name := range csvColumns {
			record[i] = values[name]
		}
		if err := out.Write(record); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// unsetSecrets lists the credential columns a profile uses but is known to
// leave empty. A profile without a credential bundle has no secrets at all.
func unsetSecrets(p config.Profile, presence map[string]service.CredentialPresence) []string {
	var set service.CredentialPresence
	if p.CredentialRef != "" {
		var known bool
		if set, known = presence[p.ID]; !known {
			return nil
		}
	}
	var unset []string
	if !set.Password {
		unset = append(unset, "password")
	}
	if s := p.Transport.SSH; s != nil && s.Auth == "password" && !set.SSHPassword {
		unset = append(unset, "ssh_password")
	}
	if s := p.Transport.SSH; s != nil && s.Auth == "key" && !set.SSHKeyPassphrase {
		unset = append(unset, "ssh_key_passphrase")
	}
	if x := p.Transport.Proxy; x != nil && x.Username != "" && !set.ProxyPassword {
		unset = append(unset, "proxy_password")
	}
	return unset
}
