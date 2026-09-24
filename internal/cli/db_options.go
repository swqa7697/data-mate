package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/vault"
	"golang.org/x/sys/unix"
)

func profileFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	for _, flag := range []struct{ name, value, help string }{
		{"driver", "postgres", "Database driver"}, {"alias", "", "Connection alias"}, {"host", "", "Database host"}, {"database", "", "Database name"}, {"username", "", "Database username"},
		{"tls-ca", "", "Absolute CA certificate path"}, {"ssh-host", "", "SSH jump host"}, {"ssh-user", "", "SSH username"}, {"ssh-key-file", "", "Import private key into the vault"}, {"proxy", "", "SOCKS5 endpoint without credentials"}, {"proxy-user", "", "Proxy username"},
	} {
		f.String(flag.name, flag.value, flag.help)
	}
	f.Int("port", 5432, "Database port")
	f.Int("ssh-port", 22, "SSH port")
	f.Bool("ssh-enroll", false, "Interactively verify and save an SSH host fingerprint")
	f.Duration("query-timeout", config.DefaultQueryTimeout, "Query timeout (1ms to 5m)")
	f.Int("max-rows", 500, "Maximum rows")
	f.Int("max-result-bytes", 1048576, "Maximum result bytes")
	for _, flag := range []struct{ name, help string }{
		{"tls", "Enable verified TLS"}, {"password-stdin", "Read one password line from stdin"}, {"credentials-stdin", "Read strict credential JSON from stdin"}, {"passwordless", "Explicitly use no database password"},
		{"clear-password", "Clear the database password"}, {"clear-ssh-password", "Clear the SSH password"}, {"clear-ssh-key-passphrase", "Clear the SSH key passphrase"}, {"clear-proxy-password", "Clear the proxy password"},
		{"clear-ssh", "Remove SSH settings and secrets"}, {"clear-proxy", "Remove proxy settings and secrets"},
	} {
		f.Bool(flag.name, false, flag.help)
	}
	cmd.MarkFlagsMutuallyExclusive("password-stdin", "credentials-stdin")
	cmd.MarkFlagsMutuallyExclusive("password-stdin", "passwordless", "clear-password")
	scopeFlags(cmd)
}
func scopeFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.Bool("all", false, "Expose all accessible tables")
	f.Bool("none", false, "Expose no tables")
	f.String("scope-json", "", "Exact nonsecret scope JSON")
	f.StringArray("schema", nil, "All current and future tables in an exact schema (repeatable)")
	f.StringArray("table", nil, "Exact schema.table selection (repeatable)")
	cmd.MarkFlagsMutuallyExclusive("all", "none", "scope-json", "schema")
	cmd.MarkFlagsMutuallyExclusive("all", "none", "scope-json", "table")
}
func str(cmd *cobra.Command, name string) string  { v, _ := cmd.Flags().GetString(name); return v }
func flag(cmd *cobra.Command, name string) bool   { v, _ := cmd.Flags().GetBool(name); return v }
func integer(cmd *cobra.Command, name string) int { v, _ := cmd.Flags().GetInt(name); return v }
func changed(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func applyOptions(cmd *cobra.Command, p *config.Profile) error {
	for _, v := range []struct {
		name   string
		target *string
	}{{"driver", &p.Driver}, {"alias", &p.Alias}, {"host", &p.Connection.Host}, {"database", &p.Connection.Database}, {"username", &p.Connection.Username}} {
		if changed(cmd, v.name) {
			*v.target = str(cmd, v.name)
		}
	}
	if changed(cmd, "port") {
		p.Connection.Port = integer(cmd, "port")
	}
	if changed(cmd, "tls") {
		if flag(cmd, "tls") {
			p.Transport.TLS.Mode = "verify-full"
		} else {
			p.Transport.TLS = config.TLS{Mode: "disabled"}
		}
	}
	if changed(cmd, "tls-ca") {
		p.Transport.TLS.CAFile = str(cmd, "tls-ca")
	}
	if flag(cmd, "clear-ssh") {
		if changed(cmd, "ssh-host", "ssh-user", "ssh-port", "ssh-key-file") {
			return invalid("SSH clear and setup flags conflict")
		}
		p.Transport.SSH = nil
	}
	if changed(cmd, "ssh-host", "ssh-user", "ssh-port", "ssh-key-file") {
		if p.Transport.SSH == nil {
			p.Transport.SSH = &config.SSH{Port: 22, Auth: "password"}
		}
		s := p.Transport.SSH
		if changed(cmd, "ssh-host") {
			s.Host = str(cmd, "ssh-host")
		}
		if changed(cmd, "ssh-user") {
			s.User = str(cmd, "ssh-user")
		}
		if changed(cmd, "ssh-port") {
			s.Port = integer(cmd, "ssh-port")
		}
		if changed(cmd, "ssh-key-file") {
			s.Auth = "key"
		}
	}
	if flag(cmd, "clear-proxy") {
		if changed(cmd, "proxy", "proxy-user") {
			return invalid("proxy clear and setup flags conflict")
		}
		p.Transport.Proxy = nil
	}
	if changed(cmd, "proxy") {
		u, err := url.Parse(str(cmd, "proxy"))
		if err != nil || u.Scheme != "socks5" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Hostname() == "" {
			return invalid("proxy must be socks5://host:port without credentials or a path")
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			return invalid("proxy requires an explicit port")
		}
		user := ""
		if p.Transport.Proxy != nil {
			user = p.Transport.Proxy.Username
		}
		p.Transport.Proxy = &config.Proxy{Kind: "socks5", Host: u.Hostname(), Port: port, Username: user}
	}
	if changed(cmd, "proxy-user") {
		if p.Transport.Proxy == nil {
			return invalid("proxy username requires a proxy")
		}
		p.Transport.Proxy.Username = str(cmd, "proxy-user")
	}
	if p.Limits == nil {
		v := config.DefaultLimits()
		p.Limits = &v
	}
	if changed(cmd, "query-timeout") {
		d, _ := cmd.Flags().GetDuration("query-timeout")
		if d < time.Millisecond || d > config.MaxQueryTimeout || d%time.Millisecond != 0 {
			return invalid("query timeout must be whole milliseconds from 1ms to 5m")
		}
		p.Limits.QueryTimeoutMS = int(d / time.Millisecond)
	}
	if changed(cmd, "max-rows") {
		p.Limits.MaxRows = integer(cmd, "max-rows")
	}
	if changed(cmd, "max-result-bytes") {
		p.Limits.MaxResultBytes = integer(cmd, "max-result-bytes")
	}
	return applyScope(cmd, p)
}
func applyScope(cmd *cobra.Command, p *config.Profile) error {
	if flag(cmd, "all") || flag(cmd, "none") || changed(cmd, "scope-json", "schema", "table") {
		p.Scope = config.Scope{Mode: "selected"}
		if flag(cmd, "all") {
			p.Scope.Mode = "all"
		}
		if changed(cmd, "scope-json") {
			b, err := contracts.JSON(strings.NewReader(str(cmd, "scope-json")), config.MaxProfileBytes)
			if err != nil || contracts.Validate("scope", b) != nil {
				return invalid("invalid scope JSON")
			}
			if json.Unmarshal(b, &p.Scope) != nil {
				return invalid("invalid scope JSON")
			}
		} else {
			p.Scope.Schemas, _ = cmd.Flags().GetStringArray("schema")
			tables, _ := cmd.Flags().GetStringArray("table")
			for _, t := range tables {
				parts := strings.Split(t, ".")
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					return invalid("table requires schema.table; use --scope-json for names containing dots")
				}
				p.Scope.Tables = append(p.Scope.Tables, config.Table{Schema: parts[0], Name: parts[1]})
			}
		}
	}

	return nil
}
func sshHost(p *config.Profile) string {
	if p.Transport.SSH != nil {
		return p.Transport.SSH.Host
	}
	return ""
}
func proxyHost(p *config.Profile) string {
	if p.Transport.Proxy != nil {
		return p.Transport.Proxy.Host
	}
	return ""
}

type secretPatch map[string]string

func readSecrets(cmd *cobra.Command) (secretPatch, error) {
	patch := secretPatch{}
	if flag(cmd, "password-stdin") {
		b, err := io.ReadAll(io.LimitReader(commandInput(cmd), vault.MaxSecretBytes+3))
		defer clear(b)
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil, cmd.Context().Err()
			}
			return nil, invalid("cannot read password")
		}
		if bytes.HasSuffix(b, []byte("\n")) {
			b = bytes.TrimSuffix(b, []byte("\n"))
			b = bytes.TrimSuffix(b, []byte("\r"))
		}
		if len(b) > vault.MaxSecretBytes || !utf8.Valid(b) || bytes.ContainsAny(b, "\r\n") {
			return nil, invalid("password input must contain one bounded UTF-8 line")
		}
		patch["password"] = string(b)
	}
	if flag(cmd, "credentials-stdin") {
		b, err := contracts.JSON(commandInput(cmd), 4*vault.MaxSecretBytes*6+128)
		defer clear(b)
		if err != nil || contracts.Validate("credentials-input", b) != nil || json.Unmarshal(b, &patch) != nil {
			if cmd.Context().Err() != nil {
				return nil, cmd.Context().Err()
			}
			return nil, invalid("invalid credential JSON")
		}
	}
	for _, entry := range []struct{ flag, key string }{{"passwordless", "password"}, {"clear-password", "password"}, {"clear-ssh-password", "ssh_password"}, {"clear-ssh-key-passphrase", "ssh_key_passphrase"}, {"clear-proxy-password", "proxy_password"}} {
		if flag(cmd, entry.flag) {
			if _, ok := patch[entry.key]; ok {
				return nil, invalid("credential input conflicts with an explicit clear")
			}
			patch[entry.key] = ""
		}
	}
	if changed(cmd, "ssh-key-file") {
		fd, err := unix.Open(str(cmd, "ssh-key-file"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, invalid("cannot read SSH key file")
		}
		f := os.NewFile(uintptr(fd), "SSH key input")
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > vault.MaxSecretBytes {
			return nil, invalid("SSH key must be a bounded regular file")
		}
		b, err := io.ReadAll(io.LimitReader(f, vault.MaxSecretBytes+1))
		defer clear(b)
		if err != nil || len(b) == 0 {
			return nil, invalid("cannot read SSH key file")
		}
		patch["ssh_private_key"] = string(b)
		patch["ssh_password"] = ""
	}
	for _, v := range patch {
		if len(v) > vault.MaxSecretBytes || !utf8.ValidString(v) {
			return nil, invalid("credential exceeds its UTF-8 byte limit")
		}
	}
	return patch, nil
}
func (p secretPatch) apply(s *vault.Secrets) {
	for k, v := range p {
		switch k {
		case "password":
			s.Password = v
		case "ssh_password":
			s.SSHPassword = v
		case "ssh_private_key":
			s.SSHPrivateKey = v
		case "ssh_key_passphrase":
			s.SSHKeyPassphrase = v
		case "proxy_password":
			s.ProxyPassword = v
		}
	}
}
