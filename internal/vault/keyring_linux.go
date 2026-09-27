package vault

import (
	"context"
	"encoding/xml"
	"errors"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

const gnomePasswordInterface = "org.gnome.keyring.InternalUnsupportedGuiltRiddenInterface"

func (s *secretSession) call(ctx context.Context, path dbus.ObjectPath, method string, flags dbus.Flags, args ...any) *dbus.Call {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.object(path).CallWithContext(bounded, method, flags, args...)
}

func (s *secretSession) checkOwner(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var owner string
	if err := s.conn.BusObject().CallWithContext(bounded, "org.freedesktop.DBus.GetNameOwner", 0, secretName).Store(&owner); err != nil || owner != s.owner {
		return secretError(ctx, ErrProviderChanged)
	}
	return nil
}

func (s *secretSession) validateCollection(ctx context.Context) error {
	if !s.collection.IsValid() || s.collection == "/" || s.collection == secretRoot+"/collection/session" {
		return ErrUnavailable
	}
	var transient dbus.ObjectPath
	if err := s.call(ctx, secretRoot, secretInterface+"Service.ReadAlias", 0, "session").Store(&transient); err != nil {
		return secretError(ctx, err)
	}
	if transient == s.collection {
		return ErrUnavailable
	}
	return s.checkOwner(ctx)
}

// Introspection is pinned to the same unique bus owner that will receive the
// password. Match argument signatures as well as method names before prompting.
func (s *secretSession) passwordCapability(ctx context.Context, method string) (supported, gnome bool, err error) {
	var raw string
	e := s.call(ctx, secretRoot, "org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&raw)
	if e != nil {
		var d dbus.Error
		if errors.As(e, &d) && (d.Name == "org.freedesktop.DBus.Error.UnknownMethod" || d.Name == "org.freedesktop.DBus.Error.UnknownInterface") {
			return false, false, nil
		}
		return false, false, secretError(ctx, e)
	}
	var node introspect.Node
	if len(raw) > 1<<20 || xml.Unmarshal([]byte(raw), &node) != nil {
		return false, false, ErrUnavailable
	}
	for _, iface := range node.Interfaces {
		if iface.Name != gnomePasswordInterface {
			continue
		}
		gnome = true
		for _, m := range iface.Methods {
			if m.Name != method {
				continue
			}
			in, out := "", ""
			for _, arg := range m.Args {
				if arg.Direction == "out" {
					out += arg.Type
				} else {
					in += arg.Type
				}
			}
			supported = method == "UnlockWithMasterPassword" && in == "o(oayays)" && out == "" || method == "CreateWithMasterPassword" && in == "a{sv}(oayays)" && out == "o"
		}
	}
	return supported, gnome, nil
}

func (s *secretSession) collectionLocked(ctx context.Context, path dbus.ObjectPath) (bool, error) {
	var value dbus.Variant
	if err := s.call(ctx, path, "org.freedesktop.DBus.Properties.Get", 0, secretInterface+"Collection", "Locked").Store(&value); err != nil {
		return false, secretError(ctx, err)
	}
	locked, ok := value.Value().(bool)
	if !ok {
		return false, ErrUnavailable
	}
	return locked, nil
}

func (s *secretSession) unlockPassword(ctx context.Context, path dbus.ObjectPath) error {
	for attempt := 1; attempt <= 3; attempt++ {
		password, err := AskKeyring(ctx, KeyringChallenge{Kind: "unlock", Attempt: attempt})
		if errors.Is(err, ErrPassword) {
			continue
		}
		if err != nil {
			return err
		}
		err = func() error {
			defer clear(password)
			if err := s.checkOwner(ctx); err != nil {
				return err
			}
			var current dbus.ObjectPath
			if err := s.call(ctx, secretRoot, secretInterface+"Service.ReadAlias", 0, "default").Store(&current); err != nil {
				return secretError(ctx, err)
			}
			if current != s.collection {
				return ErrProviderChanged
			}
			locked, err := s.collectionLocked(ctx, path)
			if err != nil || !locked {
				return err
			}
			err = s.call(ctx, secretRoot, gnomePasswordInterface+".UnlockWithMasterPassword", 0, path, busSecret{s.session, []byte{}, password, "text/plain"}).Err
			var d dbus.Error
			if errors.As(err, &d) && d.Name == "org.gnome.keyring.Error.Denied" {
				return ErrPassword
			}
			if err != nil {
				return secretError(ctx, err)
			}
			if err = s.checkOwner(ctx); err != nil {
				return err
			}
			locked, err = s.collectionLocked(ctx, path)
			if err == nil && locked {
				err = ErrPassword
			}
			return err
		}()
		if !errors.Is(err, ErrPassword) {
			return err
		}
	}
	return ErrPassword
}

func (s *secretSession) createCollection(ctx context.Context) error {
	supported, _, err := s.passwordCapability(ctx, "CreateWithMasterPassword")
	if err != nil {
		return err
	}
	if !supported {
		return ErrUnsupported
	}
	// A bus-owned lock serializes Data Mate setup across installation accounts.
	// Closing the connection releases it even after cancellation or a crash.
	const lockName = "io.github.swqa7697.DataMate.KeyringSetup"
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var acquired uint32
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.conn.BusObject().CallWithContext(bounded, "org.freedesktop.DBus.RequestName", 0, lockName, uint32(4)).Store(&acquired)
		cancel()
		if err != nil {
			return secretError(ctx, err)
		}
		if acquired == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.conn.BusObject().CallWithContext(cleanup, "org.freedesktop.DBus.ReleaseName", 0, lockName).Err
	}()
	readDefault := func() error {
		if err := s.checkOwner(ctx); err != nil {
			return err
		}
		return secretError(ctx, s.call(ctx, secretRoot, secretInterface+"Service.ReadAlias", 0, "default").Store(&s.collection))
	}
	if err = readDefault(); err != nil || s.collection != "/" {
		return err
	}
	password, err := AskKeyring(ctx, KeyringChallenge{Kind: "create", Attempt: 1})
	if err != nil {
		return err
	}
	defer clear(password)
	// An external provider client may have established a default while the user
	// typed. Reuse it, and never pass the proposed new password as its password.
	if err = readDefault(); err != nil || s.collection != "/" {
		return err
	}
	var created dbus.ObjectPath
	properties := map[string]dbus.Variant{secretInterface + "Collection.Label": dbus.MakeVariant("Data Mate")}
	if err = s.call(ctx, secretRoot, gnomePasswordInterface+".CreateWithMasterPassword", 0, properties, busSecret{s.session, []byte{}, password, "text/plain"}).Store(&created); err != nil {
		return secretError(ctx, err)
	}
	if !created.IsValid() || created == "/" || created == secretRoot+"/collection/session" {
		return ErrUnavailable
	}
	// Preserve the protected collection on failure: it can contain recoverable
	// resources. SetAlias has no compare-and-swap; recheck immediately before it.
	if err = readDefault(); err != nil || s.collection != "/" {
		return err
	}
	if err = s.call(ctx, secretRoot, secretInterface+"Service.SetAlias", 0, "default", created).Err; err != nil {
		return secretError(ctx, err)
	}
	if err = readDefault(); err != nil {
		return err
	}
	if s.collection != created {
		return ErrProviderChanged
	}
	locked, err := s.collectionLocked(ctx, created)
	if err == nil && locked {
		return ErrLocked
	}
	return err
}

// PrepareDelete unlocks and verifies an existing item without exposing its keyset
// or deleting anything. Cleanup later uses a noninteractive exact deletion.
func (k SecretService) PrepareDelete(ctx context.Context, account string) error {
	s, err := k.open(ctx, account)
	if err != nil {
		return err
	}
	defer s.close()
	_, err = s.find(ctx, account)
	if errors.Is(err, ErrMissing) {
		return nil
	}
	return err
}

func busCall(ctx context.Context, conn *dbus.Conn, method string, flags dbus.Flags, args ...any) *dbus.Call {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return conn.BusObject().CallWithContext(bounded, method, flags, args...)
}
