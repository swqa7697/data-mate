package vault

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/swqa7697/data-mate/internal/testsupport/dbusfixture"
)

// The repository's fake KeyProvider cannot exercise native D-Bus decoding,
// locked collections, or duplicate search results. This boundary scenario uses
// a private bus without host activation files and a synthetic service; the real
// keyring remains opt-in. A running service must not require an activation file.
func TestSecretServiceBoundary(t *testing.T) {
	for _, scenario := range []string{"lifecycle", "unavailable", "locked", "denied", "canceled-prompt", "ambiguous", "wrong-account", "unexpected-attribute", "oversized", "wrong-session", "session-collection", "terminal-unlock", "wrong-password", "terminal-cancel", "provider-replaced", "first-setup", "setup-default-race", "setup-concurrent", "unsupported", "noninteractive"} {
		t.Run(scenario, func(t *testing.T) {
			bus := dbusfixture.Start(t)
			server, err := bus.Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			// No owner or activation file must still fail closed.
			if scenario != "unavailable" {
				if reply, err := server.RequestName(secretName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
					t.Fatalf("own synthetic service: reply=%v err=%v", reply, err)
				}
			}
			collection := dbus.ObjectPath("/org/freedesktop/secrets/collection/login")
			item := dbus.ObjectPath(string(collection) + "/one")
			session := dbus.ObjectPath("/org/freedesktop/secrets/session/one")
			prompt := dbus.ObjectPath("/org/freedesktop/secrets/prompt/one")
			account := strings.Repeat("a", 64)
			var mu sync.Mutex
			setup := strings.HasPrefix(scenario, "setup-") || scenario == "first-setup"
			exists := scenario != "lifecycle" && !setup
			hasDefault := !setup
			creates, passwords, terminalPrompts := 0, 0, 0
			key := []byte("synthetic-keyset")
			terminalScenario := scenario == "terminal-unlock" || scenario == "wrong-password" || scenario == "terminal-cancel" || scenario == "provider-replaced" || scenario == "unsupported" || scenario == "noninteractive"
			locked := terminalScenario || scenario == "locked" || scenario == "denied" || scenario == "canceled-prompt"
			promptCalls := 0
			export := func(path dbus.ObjectPath, iface string, methods map[string]any) {
				t.Helper()
				if err := server.ExportMethodTable(methods, path, iface); err != nil {
					t.Fatal(err)
				}
			}
			export(secretRoot, secretInterface+"Service", map[string]any{
				"ReadAlias": func(alias string) (dbus.ObjectPath, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					if alias == "default" && !hasDefault {
						return "/", nil
					}
					if alias == "session" {
						return secretRoot + "/collection/session", nil
					}
					if scenario == "session-collection" {
						return secretRoot + "/collection/session", nil
					}
					return collection, nil
				},
				"SetAlias": func(alias string, path dbus.ObjectPath) *dbus.Error {
					mu.Lock()
					defer mu.Unlock()
					if hasDefault || path != collection {
						return dbus.NewError("org.freedesktop.DBus.Error.Failed", nil)
					}
					hasDefault = true
					return nil
				},
				"OpenSession": func(algorithm string, input dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
					return dbus.MakeVariant(""), session, nil
				},
				"Unlock": func(paths []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
					return []dbus.ObjectPath{}, prompt, nil
				},
			})
			if setup || terminalScenario {
				export(secretRoot, "org.freedesktop.DBus.Introspectable", map[string]any{
					"Introspect": func() (string, *dbus.Error) {
						if scenario == "unsupported" {
							return `<node><interface name="` + gnomePasswordInterface + `"/></node>`, nil
						}
						return `<node><interface name="` + gnomePasswordInterface + `"><method name="UnlockWithMasterPassword"><arg type="o" direction="in"/><arg type="(oayays)" direction="in"/></method><method name="CreateWithMasterPassword"><arg type="a{sv}" direction="in"/><arg type="(oayays)" direction="in"/><arg type="o" direction="out"/></method></interface></node>`, nil
					},
				})
				export(secretRoot, gnomePasswordInterface, map[string]any{
					"UnlockWithMasterPassword": func(path dbus.ObjectPath, secret busSecret) *dbus.Error {
						mu.Lock()
						defer mu.Unlock()
						passwords++
						if path != collection || secret.Session != session || string(secret.Value) != "synthetic-password" {
							return dbus.NewError("org.gnome.keyring.Error.Denied", []any{"unsafe upstream text"})
						}
						locked = false
						return nil
					},
					"CreateWithMasterPassword": func(properties map[string]dbus.Variant, secret busSecret) (dbus.ObjectPath, *dbus.Error) {
						mu.Lock()
						defer mu.Unlock()
						creates++
						if string(secret.Value) != "synthetic-password" || properties[secretInterface+"Collection.Label"].Value() != "Data Mate" {
							return "/", dbus.NewError("org.freedesktop.DBus.Error.Failed", nil)
						}
						locked = false
						return collection, nil
					},
				})
			}
			export(collection, secretInterface+"Collection", map[string]any{
				"SearchItems": func(attributes map[string]string) ([]dbus.ObjectPath, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					if !exists {
						return []dbus.ObjectPath{}, nil
					}
					if scenario == "ambiguous" {
						return []dbus.ObjectPath{item, item}, nil
					}
					return []dbus.ObjectPath{item}, nil
				},
				"CreateItem": func(properties map[string]dbus.Variant, secret busSecret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					if replace || exists {
						return "/", "/", dbus.NewError("org.freedesktop.DBus.Error.Failed", nil)
					}
					exists = true
					key = append([]byte{}, secret.Value...)
					return item, "/", nil
				},
			})
			export(collection, "org.freedesktop.DBus.Properties", map[string]any{
				"Get": func(iface, name string) (dbus.Variant, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					return dbus.MakeVariant(locked), nil
				},
			})
			export(item, "org.freedesktop.DBus.Properties", map[string]any{
				"Get": func(iface, name string) (dbus.Variant, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					attributes := secretAttributes(account)
					// GNOME exposes hashed lookup fields while locked, and adds a
					// schema after reloading an item from persistent storage.
					if locked {
						attributes = map[string]string{"gkr:compat:hashed:application": "synthetic-hash", "gkr:compat:hashed:account": "synthetic-hash"}
					}
					if scenario != "lifecycle" || locked || promptCalls > 0 {
						attributes["xdg:schema"] = "org.freedesktop.Secret.Generic"
					}
					if scenario == "wrong-account" {
						attributes["account"] = strings.Repeat("b", 64)
					}
					if scenario == "unexpected-attribute" {
						attributes["unexpected"] = "synthetic"
					}
					return dbus.MakeVariant(attributes), nil
				},
			})
			export(item, secretInterface+"Item", map[string]any{
				"GetSecret": func(requested dbus.ObjectPath) (busSecret, *dbus.Error) {
					mu.Lock()
					defer mu.Unlock()
					raw := append([]byte{}, key...)
					returned := session
					if scenario == "oversized" {
						raw = make([]byte, MaxKeysetBytes+1)
					}
					if scenario == "wrong-session" {
						returned = "/wrong"
					}
					return busSecret{returned, []byte{}, raw, "text/plain"}, nil
				},
				"Delete": func() (dbus.ObjectPath, *dbus.Error) { mu.Lock(); defer mu.Unlock(); exists = false; return "/", nil },
			})
			prompted := make(chan struct{}, 1)
			export(prompt, secretInterface+"Prompt", map[string]any{
				"Prompt": func(window string) *dbus.Error {
					mu.Lock()
					promptCalls++
					if scenario == "lifecycle" {
						locked = false
					}
					mu.Unlock()
					prompted <- struct{}{}
					if scenario == "denied" {
						_ = server.Emit(prompt, secretInterface+"Prompt.Completed", true, dbus.MakeVariant([]dbus.ObjectPath{}))
					} else if scenario == "lifecycle" {
						_ = server.Emit(prompt, secretInterface+"Prompt.Completed", false, dbus.MakeVariant([]dbus.ObjectPath{collection}))
					}
					return nil
				},
				"Dismiss": func() *dbus.Error { return nil },
			})
			k := SecretService{connect: bus.Connect}
			ctx := t.Context()
			if scenario == "denied" || scenario == "canceled-prompt" {
				ctx = WithInteraction(ctx, true)
			}
			if terminalScenario || setup {
				ctx = WithKeyringPrompt(WithInteraction(ctx, scenario != "noninteractive"), func(_ context.Context, c KeyringChallenge) ([]byte, error) {
					mu.Lock()
					terminalPrompts++
					count := terminalPrompts
					mu.Unlock()
					if scenario == "terminal-cancel" {
						return nil, context.Canceled
					}
					if scenario == "provider-replaced" {
						if _, e := server.ReleaseName(secretName); e != nil {
							t.Error(e)
						}
					}
					if scenario == "setup-default-race" {
						mu.Lock()
						hasDefault = true
						mu.Unlock()
					}
					if scenario == "wrong-password" || scenario == "terminal-unlock" && count == 1 {
						return []byte("synthetic-wrong"), nil
					}
					return []byte("synthetic-password"), nil
				})
				if scenario == "setup-concurrent" {
					results := make(chan error, 2)
					for _, a := range []string{account, strings.Repeat("b", 64)} {
						go func() {
							session, e := k.openCollection(ctx, a, true)
							if e == nil {
								session.close()
							}
							results <- e
						}()
					}
					for i := 0; i < 2; i++ {
						if e := <-results; e != nil {
							t.Fatal(e)
						}
					}
				} else if setup {
					_, err = k.CreateIfAbsent(ctx, account, []byte("synthetic-keyset"))
				} else {
					_, err = k.Load(ctx, account)
				}
				want := error(nil)
				switch scenario {
				case "wrong-password":
					want = ErrPassword
				case "terminal-cancel":
					want = context.Canceled
				case "provider-replaced":
					want = ErrProviderChanged
				case "unsupported":
					want = ErrUnsupported
				case "noninteractive":
					want = ErrLocked
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v want %v", err, want)
				}
				mu.Lock()
				defer mu.Unlock()
				switch scenario {
				case "terminal-unlock":
					if passwords != 2 || terminalPrompts != 2 || locked {
						t.Fatal("retry did not unlock exactly once")
					}
				case "wrong-password":
					if passwords != 3 || terminalPrompts != 3 {
						t.Fatal("password attempt bound")
					}
				case "terminal-cancel", "provider-replaced", "unsupported", "noninteractive":
					if passwords != 0 || creates != 0 {
						t.Fatal("aborted preparation touched protected resources")
					}
				case "first-setup", "setup-concurrent":
					if creates != 1 || terminalPrompts != 1 || !hasDefault {
						t.Fatal("setup was duplicated", creates, terminalPrompts)
					}
				case "setup-default-race":
					if creates != 0 || !hasDefault {
						t.Fatal("replaced concurrently established default")
					}
				}
				return
			}
			if scenario == "lifecycle" {
				if _, err = k.Load(ctx, account); !errors.Is(err, ErrMissing) {
					t.Fatal("missing", err)
				}
				first, err := k.CreateIfAbsent(ctx, account, []byte("first"))
				if err != nil || string(first) != "first" {
					t.Fatal("create", err)
				}
				mu.Lock()
				locked = true
				mu.Unlock()
				second, err := k.CreateIfAbsent(WithInteraction(ctx, true), account, []byte("second"))
				if err != nil || !bytes.Equal(first, second) {
					t.Fatal("existing key overwritten", err)
				}
				if err = k.Delete(ctx, account); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				calls := promptCalls
				mu.Unlock()
				if calls != 1 {
					t.Fatalf("expected one unlock before verifying item identity, got %d", calls)
				}
				if err = k.Delete(ctx, account); err != nil {
					t.Fatal(err)
				}
				return
			}
			want := ErrUnavailable
			switch scenario {
			case "unavailable":
				want = ErrProviderUnavailable
			case "locked":
				want = ErrLocked
			case "denied":
				want = ErrDenied
			case "canceled-prompt":
				want = context.Canceled
			}
			if scenario == "canceled-prompt" {
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := k.Load(cancelCtx, account); result <- err }()
				select {
				case <-prompted:
					cancel()
				case <-time.After(3 * time.Second):
					t.Fatal("prompt not reached")
				}
				select {
				case err = <-result:
				case <-time.After(3 * time.Second):
					t.Fatal("cancellation blocked")
				}
			} else {
				_, err = k.Load(ctx, account)
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			mu.Lock()
			calls := promptCalls
			mu.Unlock()
			if scenario == "locked" && calls != 0 {
				t.Fatal("unattended request prompted")
			}
		})
	}
}
