// The opt-in fixture uses the production vault and native adapter, with only
// synthetic credentials. Root and result arguments contain no secret material.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/swqa7697/data-mate/internal/cli"
	"github.com/swqa7697/data-mate/internal/service"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

var buildIdentity = "original"
var version = "native"
var revision = "fixture"

const secret = "synthetic-native-P1-credential"

func main() {
	syscall.Umask(0077)
	if len(os.Args) > 1 && os.Args[1] != "create" && os.Args[1] != "verify" && os.Args[1] != "purge" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if os.Args[1] == "__service" {
			flags := flag.NewFlagSet("fixture-service", flag.ContinueOnError)
			rootPath := flags.String("root", "", "")
			nonce := flags.String("instance", "", "")
			if flags.Parse(os.Args[2:]) != nil {
				os.Exit(2)
			}
			root, e := config.ResolveRoot(*rootPath, "")
			if e != nil {
				os.Exit(2)
			}
			build, e := service.ExecutableBuild(version, revision)
			if e != nil {
				os.Exit(2)
			}
			if e = service.Serve(ctx, root, build, *nonce, observedKeys{root.Path + ".key-access"}); e != nil {
				os.Exit(1)
			}
			return
		}
		os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.Build{Version: version, Revision: revision}))
	}

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
	defer repo.Close()
	account := ""
	if mode != "purge" {
		lease, e := store.ReadLease(ctx)
		if e != nil {
			return e
		}
		account = lease.Identity().KeyAccount
		lease.Release()
	}

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

		original, err := keys.Load(ctx, account)
		if err != nil {
			return err
		}
		defer clear(original)
		duplicate, err := keys.CreateIfAbsent(ctx, account, bytes.Repeat([]byte{9}, 32))
		if err != nil {
			return err
		}
		defer clear(duplicate)
		if !bytes.Equal(original, duplicate) {
			return fmt.Errorf("create-if-absent replaced an existing key")
		}
	case "verify":
		if err := repo.Unlock(ctx, false, ""); err != nil {
			return err
		}
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

// This fixture-only wrapper records operation names, never key material.
type observedKeys struct{ path string }

func (k observedKeys) record(op string) error {
	f, e := os.OpenFile(k.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.WriteString(op + "\n")
	return e
}
func (k observedKeys) Load(ctx context.Context, account string) ([]byte, error) {
	if e := k.record("load"); e != nil {
		return nil, vault.ErrUnavailable
	}
	return (vault.Keychain{}).Load(ctx, account)
}
func (k observedKeys) CreateIfAbsent(ctx context.Context, account string, raw []byte) ([]byte, error) {
	if e := k.record("create"); e != nil {
		return nil, vault.ErrUnavailable
	}
	return (vault.Keychain{}).CreateIfAbsent(ctx, account, raw)
}
func (k observedKeys) Delete(ctx context.Context, account string) error {
	if e := k.record("delete"); e != nil {
		return vault.ErrUnavailable
	}
	return (vault.Keychain{}).Delete(ctx, account)
}
