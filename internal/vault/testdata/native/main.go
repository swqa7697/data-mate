// The opt-in fixture uses the production vault and native adapter, with only
// synthetic credentials. Root and result arguments contain no secret material.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

var buildIdentity = "original"

const secret = "synthetic-native-P1-credential"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, vault.ErrDenied) || errors.Is(err, vault.ErrLocked) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("expected fixture mode, isolated root, result file")
	}
	mode, rootPath, result := os.Args[1], os.Args[2], os.Args[3]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, err := config.ResolveRoot(rootPath, "")
	if err != nil {
		return err
	}
	store, err := config.Open(ctx, root, nil)
	if err != nil {
		return err
	}
	defer store.Close()
	keys := vault.Keychain{} // unattended: native approval remains required if denied
	repo := vault.New(store, keys)
	switch mode {
	case "create":
		p, rev, err := repo.Snapshot(ctx)
		if err != nil {
			return err
		}
		replacements := map[string]vault.Secrets{}
		for i := 0; i < 3; i++ {
			id, err := config.NewID()
			if err != nil {
				return err
			}
			p.Connections = append(p.Connections, config.Profile{ID: id, Alias: fmt.Sprintf("native-%d", i), Driver: "postgres", Connection: config.Connection{Host: "127.0.0.1", Port: 5432, Database: "fixture", Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}, Scope: config.Scope{Mode: "all"}})
			replacements[id] = vault.Secrets{Password: secret}
		}
		if _, err := repo.Apply(ctx, vault.Mutation{Expected: rev, Profiles: p, Replacements: replacements}); err != nil {
			return err
		}

		original, err := keys.Load(ctx, root.Digest)
		if err != nil {
			return err
		}
		defer clear(original)
		duplicate, err := keys.CreateIfAbsent(ctx, root.Digest, bytes.Repeat([]byte{9}, 32))
		if err != nil {
			return err
		}
		defer clear(duplicate)
		if !bytes.Equal(original, duplicate) {
			return fmt.Errorf("create-if-absent replaced an existing key")
		}
	case "verify":
		l, err := store.ReadLease(ctx)
		if err != nil {
			return err
		}
		defer l.Release()
		p, _, err := l.ProfileSnapshot()
		if err != nil {
			return err
		}
		if len(p.Connections) != 3 {
			return fmt.Errorf("native profile count")
		}
		for _, c := range p.Connections {
			s, err := repo.Credential(ctx, l, c.ID)
			if err != nil {
				return err
			}
			if s.Password != secret {
				return fmt.Errorf("native credential mismatch")
			}
		}
	case "purge":
		if err := repo.PurgeCredentials(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid native mode")
	}
	return os.WriteFile(result, []byte("passed "+mode+" "+buildIdentity+"\n"), 0600)
}
