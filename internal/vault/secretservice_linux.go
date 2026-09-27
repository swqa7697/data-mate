package vault

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/swqa7697/data-mate/internal/nativebus"
)

const secretName = "org.freedesktop.secrets"
const secretRoot dbus.ObjectPath = "/org/freedesktop/secrets"
const secretInterface = "org.freedesktop.Secret."

// SecretService keeps the serialized Tink keyset in the persistent default
// collection. The plain protocol session is confined to the authenticated local
// Unix user bus; keysets never enter a subprocess, environment or application file.
type SecretService struct {
	connect func(context.Context) (*dbus.Conn, error)
}

// NewKeyProvider selects the native secure store without accessing it.
func NewKeyProvider() KeyProvider { return SecretService{connect: nativebus.Connect} }

type secretSession struct {
	conn       *dbus.Conn
	owner      string
	collection dbus.ObjectPath
	session    dbus.ObjectPath
}
type busSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func secretError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	var e dbus.Error
	if errors.As(err, &e) {
		switch e.Name {
		case secretInterface + "Error.IsLocked":
			return ErrLocked
		case "org.freedesktop.DBus.Error.AccessDenied":
			return ErrDenied
		case secretInterface + "Error.NoSuchObject":
			return ErrMissing
		}
	}
	return providerError(err)
}
func (s *secretSession) object(path dbus.ObjectPath) dbus.BusObject {
	return s.conn.Object(s.owner, path)
}
func (s *secretSession) close() { s.conn.Close() }
func (k SecretService) open(ctx context.Context, account string) (*secretSession, error) {
	if !validFingerprint(account) {
		return nil, ErrUnavailable
	}
	connect := k.connect
	if connect == nil {
		connect = nativebus.Connect
	}
	conn, err := connect(ctx)
	if err != nil {
		return nil, secretError(ctx, err)
	}
	fail := func(err error) (*secretSession, error) { conn.Close(); return nil, secretError(ctx, err) }
	// Serialize search/create/delete across processes, including direct native
	// probes. Do not queue or replace another caller's lock ownership.
	var acquired uint32
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.RequestName", 0, "io.github.swqa7697.DataMate.Keyset.k"+account, uint32(4)).Store(&acquired); err != nil || acquired != 1 {
		return fail(ErrUnavailable)
	}
	s := &secretSession{conn: conn}
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, secretName).Store(&s.owner)
	var busErr dbus.Error
	if errors.As(err, &busErr) && busErr.Name == "org.freedesktop.DBus.Error.NameHasNoOwner" {
		// A running service need not have an activation file. Activate only
		// when absent, then pin the owner for the entire operation.
		if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.StartServiceByName", 0, secretName, uint32(0)).Err; err != nil {
			return fail(err)
		}
		err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, secretName).Store(&s.owner)
	}
	if err != nil {
		return fail(err)
	}
	if err = s.object(secretRoot).CallWithContext(ctx, secretInterface+"Service.ReadAlias", 0, "default").Store(&s.collection); err != nil {
		return fail(err)
	}
	if !s.collection.IsValid() || s.collection == "/" || s.collection == secretRoot+"/collection/session" {
		return fail(ErrUnavailable)
	}
	// Never create an unprotected collection implicitly or use the session keyring.
	var transient dbus.ObjectPath
	if err = s.object(secretRoot).CallWithContext(ctx, secretInterface+"Service.ReadAlias", 0, "session").Store(&transient); err != nil {
		return fail(err)
	}
	if transient == s.collection {
		return fail(ErrUnavailable)
	}
	var output dbus.Variant
	if err = s.object(secretRoot).CallWithContext(ctx, secretInterface+"Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &s.session); err != nil {
		return fail(err)
	}
	if !s.session.IsValid() || s.session == "/" {
		return fail(ErrUnavailable)
	}
	return s, nil
}
func (s *secretSession) prompt(ctx context.Context, path dbus.ObjectPath) error {
	if path == "/" {
		return nil
	}
	allowed, _ := ctx.Value(interactionKey{}).(bool)
	if !allowed {
		return ErrLocked
	}
	signals := make(chan *dbus.Signal, 4)
	s.conn.Signal(signals)
	defer s.conn.RemoveSignal(signals)
	opts := []dbus.MatchOption{dbus.WithMatchSender(s.owner), dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(secretInterface + "Prompt"), dbus.WithMatchMember("Completed")}
	if err := s.conn.AddMatchSignalContext(ctx, opts...); err != nil {
		return secretError(ctx, err)
	}
	defer s.conn.RemoveMatchSignal(opts...)
	if err := s.object(path).CallWithContext(ctx, secretInterface+"Prompt.Prompt", 0, "").Err; err != nil {
		return secretError(ctx, err)
	}
	for {
		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = s.object(path).CallWithContext(cleanup, secretInterface+"Prompt.Dismiss", 0).Err
			cancel()
			return ctx.Err()
		case signal := <-signals:
			if signal == nil {
				return ErrUnavailable
			}
			if signal.Path != path || signal.Name != secretInterface+"Prompt.Completed" || signal.Sender != s.owner {
				continue
			}
			var dismissed bool
			var result dbus.Variant
			if dbus.Store(signal.Body, &dismissed, &result) != nil {
				return ErrUnavailable
			}
			if dismissed {
				return ErrDenied
			}
			return nil
		}
	}
}
func (s *secretSession) unlock(ctx context.Context, path dbus.ObjectPath) error {
	var locked dbus.Variant
	if err := s.object(path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, secretInterface+"Collection", "Locked").Store(&locked); err != nil {
		return secretError(ctx, err)
	}
	if locked.Value() == false {
		return nil
	}
	if locked.Value() != true {
		return ErrUnavailable
	}
	allowed, _ := ctx.Value(interactionKey{}).(bool)
	if !allowed {
		return ErrLocked
	}
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := s.object(secretRoot).CallWithContext(ctx, secretInterface+"Service.Unlock", 0, []dbus.ObjectPath{path}).Store(&unlocked, &prompt); err != nil {
		return secretError(ctx, err)
	}
	if err := s.prompt(ctx, prompt); err != nil {
		return err
	}
	if err := s.object(path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, secretInterface+"Collection", "Locked").Store(&locked); err != nil {
		return secretError(ctx, err)
	}
	if locked.Value() != false {
		return ErrLocked
	}
	return nil
}
func secretAttributes(account string) map[string]string {
	return map[string]string{"application": "io.github.swqa7697.data-mate", "account": account}
}
func (s *secretSession) find(ctx context.Context, account string) (dbus.ObjectPath, error) {
	var items []dbus.ObjectPath
	if err := s.object(s.collection).CallWithContext(ctx, secretInterface+"Collection.SearchItems", 0, secretAttributes(account)).Store(&items); err != nil {
		return "", secretError(ctx, err)
	}
	if len(items) == 0 {
		return "", ErrMissing
	}
	if len(items) != 1 || !items[0].IsValid() || !strings.HasPrefix(string(items[0]), string(s.collection)+"/") {
		return "", ErrUnavailable
	}
	var attributes dbus.Variant
	if err := s.object(items[0]).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, secretInterface+"Item", "Attributes").Store(&attributes); err != nil {
		return "", secretError(ctx, err)
	}
	actual, ok := attributes.Value().(map[string]string)
	if !ok || len(actual) != 2 || actual["application"] != secretAttributes(account)["application"] || actual["account"] != account {
		return "", ErrUnavailable
	}
	return items[0], nil
}
func (s *secretSession) load(ctx context.Context, item dbus.ObjectPath) ([]byte, error) {
	if err := s.unlock(ctx, s.collection); err != nil {
		return nil, err
	}
	var secret busSecret
	if err := s.object(item).CallWithContext(ctx, secretInterface+"Item.GetSecret", 0, s.session).Store(&secret); err != nil {
		return nil, secretError(ctx, err)
	}
	if secret.Session != s.session || len(secret.Parameters) != 0 || len(secret.Value) == 0 || len(secret.Value) > MaxKeysetBytes || ctx.Err() != nil {
		clear(secret.Value)
		return nil, secretError(ctx, ErrUnavailable)
	}
	return secret.Value, nil
}
func (k SecretService) Load(parent context.Context, account string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	s, err := k.open(ctx, account)
	if err != nil {
		return nil, err
	}
	defer s.close()
	item, err := s.find(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, item)
}
func (k SecretService) CreateIfAbsent(parent context.Context, account string, key []byte) ([]byte, error) {
	if len(key) == 0 || len(key) > MaxKeysetBytes {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	s, err := k.open(ctx, account)
	if err != nil {
		return nil, err
	}
	defer s.close()
	item, err := s.find(ctx, account)
	if err == nil {
		return s.load(ctx, item)
	}
	if !errors.Is(err, ErrMissing) {
		return nil, err
	}
	if err = s.unlock(ctx, s.collection); err != nil {
		return nil, err
	}
	properties := map[string]dbus.Variant{secretInterface + "Item.Label": dbus.MakeVariant("Data Mate installation keyset"), secretInterface + "Item.Attributes": dbus.MakeVariant(secretAttributes(account))}
	var prompt dbus.ObjectPath
	// replace=false is essential: even an unexpected competing writer cannot
	// cause us to overwrite existing key material. Ambiguity is detected below.
	err = s.object(s.collection).CallWithContext(ctx, secretInterface+"Collection.CreateItem", 0, properties, busSecret{s.session, []byte{}, key, "application/octet-stream"}, false).Store(&item, &prompt)
	if err != nil {
		return nil, secretError(ctx, err)
	}
	if err = s.prompt(ctx, prompt); err != nil {
		return nil, err
	}
	found, err := s.find(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, found)
}
func (k SecretService) Delete(parent context.Context, account string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	s, err := k.open(ctx, account)
	if err != nil {
		return err
	}
	defer s.close()
	item, err := s.find(ctx, account)
	if errors.Is(err, ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = s.unlock(ctx, s.collection); err != nil {
		return err
	}
	var prompt dbus.ObjectPath
	if err = s.object(item).CallWithContext(ctx, secretInterface+"Item.Delete", 0).Store(&prompt); err != nil {
		return secretError(ctx, err)
	}
	if err = s.prompt(ctx, prompt); err != nil {
		return err
	}
	_, err = s.find(ctx, account)
	if errors.Is(err, ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrUnavailable
}
