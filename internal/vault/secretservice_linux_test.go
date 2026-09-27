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
	for _, scenario := range []string{"lifecycle", "unavailable", "locked", "denied", "canceled-prompt", "ambiguous", "wrong-account", "unexpected-attribute", "oversized", "wrong-session", "session-collection"} {
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
			exists := scenario != "lifecycle"
			key := []byte("synthetic-keyset")
			locked := scenario == "locked" || scenario == "denied" || scenario == "canceled-prompt"
			promptCalls := 0
			export := func(path dbus.ObjectPath, iface string, methods map[string]any) {
				t.Helper()
				if err := server.ExportMethodTable(methods, path, iface); err != nil {
					t.Fatal(err)
				}
			}
			export(secretRoot, secretInterface+"Service", map[string]any{
				"ReadAlias": func(alias string) (dbus.ObjectPath, *dbus.Error) {
					if alias == "session" {
						return secretRoot + "/collection/session", nil
					}
					if scenario == "session-collection" {
						return secretRoot + "/collection/session", nil
					}
					return collection, nil
				},
				"OpenSession": func(algorithm string, input dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
					return dbus.MakeVariant(""), session, nil
				},
				"Unlock": func(paths []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
					return []dbus.ObjectPath{}, prompt, nil
				},
			})
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
