package cli

import (
	"encoding/json"
	"github.com/swqa7697/data-mate/internal/vault"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/swqa7697/data-mate/internal/config"
)

// collectProfile runs before opening persistent state or accessing the key store.
func collectProfile(cmd *cobra.Command, action string, profile, original config.Profile, getForm func() (*form, error)) (config.Profile, secretPatch, error) {
	stdinMode := flag(cmd, "password-stdin") || flag(cmd, "credentials-stdin")
	// Detach pointers so the original remains the credential/snapshot authority.
	b, _ := json.Marshal(profile)
	profile = config.Profile{}
	_ = json.Unmarshal(b, &profile)
	if err := applyOptions(cmd, &profile); err != nil {
		return profile, nil, err
	}
	patch, err := readSecrets(cmd)
	if err != nil {
		return profile, nil, err
	}
	// With no editing flags, present the basic form. Flag-based edits preserve omissions.
	fullForm := !flag(cmd, "yes") && !stdinMode
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name != "root" {
			fullForm = false
		}
	})
	for _, field := range []struct {
		name, label string
		value       *string
	}{
		{"driver", "Driver", &profile.Driver}, {"alias", "Alias", &profile.Alias}, {"host", "Host", &profile.Connection.Host},
		{"port", "Port", nil}, {"database", "Database", &profile.Connection.Database}, {"username", "Username", &profile.Connection.Username},
	} {
		missing := field.value != nil && *field.value == ""
		if (fullForm && !changed(cmd, field.name)) || missing {
			f, err := getForm()
			if err != nil {
				return profile, nil, err
			}
			current := strconv.Itoa(profile.Connection.Port)
			if field.value != nil {
				current = *field.value
			}
			value, err := f.ask(field.label, current, false)
			if err != nil {
				return profile, nil, err
			}
			if field.value == nil {
				n, e := strconv.Atoi(value)
				if e != nil {
					return profile, nil, invalid("invalid port")
				}
				profile.Connection.Port = n
			} else {
				*field.value = value
			}
		}
	}
	if _, ok := patch["password"]; !ok && (action == "add" || fullForm) {
		f, err := getForm()
		if err != nil {
			return profile, nil, invalid("add requires --password-stdin, --credentials-stdin with password, or --passwordless")
		}
		label := "Password"
		if action == "edit" {
			label = "Password (blank keeps existing; --clear-password removes)"
		}
		value, err := f.ask(label, "", true)
		if err != nil {
			return profile, nil, err
		}
		if action == "add" || value != "" {
			patch["password"] = value
		}
	}
	if profile.Transport.SSH != nil {
		for _, field := range []struct {
			label string
			value *string
		}{{"SSH host", &profile.Transport.SSH.Host}, {"SSH username", &profile.Transport.SSH.User}} {
			if *field.value == "" {
				f, e := getForm()
				if e != nil {
					return profile, nil, e
				}
				v, e := f.ask(field.label, "", false)
				if e != nil {
					return profile, nil, e
				}
				*field.value = v
			}
		}
		if profile.Transport.SSH.Auth == "password" && (original.Transport.SSH == nil || original.Transport.SSH.Auth != "password") {
			if _, ok := patch["ssh_password"]; !ok {
				f, e := getForm()
				if e != nil {
					return profile, nil, invalid("SSH setup requires ssh_password in --credentials-stdin")
				}
				v, e := f.ask("SSH password", "", true)
				if e != nil {
					return profile, nil, e
				}
				patch["ssh_password"] = v
			}
		}
	}
	if changed(cmd, "ssh-key-file") && !flag(cmd, "yes") && !stdinMode {
		if _, ok := patch["ssh_key_passphrase"]; !ok {
			f, e := getForm()
			if e != nil {
				return profile, nil, e
			}
			value, e := f.ask("SSH key passphrase (blank for none)", "", true)
			if e != nil {
				return profile, nil, e
			}
			patch["ssh_key_passphrase"] = value
		}
	}
	if profile.Transport.Proxy != nil && profile.Transport.Proxy.Username != "" && (original.Transport.Proxy == nil || original.Transport.Proxy.Username == "") {
		if _, ok := patch["proxy_password"]; !ok {
			f, e := getForm()
			if e != nil {
				return profile, nil, invalid("authenticated proxy setup requires proxy_password in --credentials-stdin")
			}
			v, e := f.ask("Proxy password", "", true)
			if e != nil {
				return profile, nil, e
			}
			patch["proxy_password"] = v
		}
	}
	if flag(cmd, "clear-ssh") {
		if patch["ssh_password"] != "" || patch["ssh_private_key"] != "" || patch["ssh_key_passphrase"] != "" {
			return profile, nil, invalid("SSH credentials conflict with --clear-ssh")
		}
		patch["ssh_password"] = ""
		patch["ssh_private_key"] = ""
		patch["ssh_key_passphrase"] = ""
	}
	if flag(cmd, "clear-proxy") {
		if patch["proxy_password"] != "" {
			return profile, nil, invalid("proxy credentials conflict with --clear-proxy")
		}
		patch["proxy_password"] = ""
	}
	for key, value := range patch {
		if len(value) > vault.MaxSecretBytes || !utf8.ValidString(value) {
			return profile, nil, invalid("credential exceeds its UTF-8 byte limit")
		}
		if value != "" && ((strings.HasPrefix(key, "ssh_") && profile.Transport.SSH == nil) || (key == "proxy_password" && profile.Transport.Proxy == nil)) {
			return profile, nil, invalid("credential requires its matching transport")
		}
	}
	for _, host := range []string{profile.Connection.Host, sshHost(&profile), proxyHost(&profile)} {
		if strings.ContainsAny(host, "/@?=#\\\r\n\t ") {
			return profile, nil, invalid("endpoint must be a hostname or IP address")
		}
	}

	return profile, patch, nil
}
