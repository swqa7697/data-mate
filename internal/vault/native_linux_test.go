package vault

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/testsupport/dbusfixture"
)

// Linux counterpart of the native Keychain lifecycle: a real Secret Service
// daemon has its own bus, home and synthetic keyring password. No desktop
// collection, existing credential, or global environment is accessed.
func TestNativeSecretServiceLifecycle(t *testing.T) {
	if os.Getenv("DATA_MATE_NATIVE_TEST") != "1" {
		t.Skip("requires DATA_MATE_NATIVE_TEST=1, dbus-daemon and gnome-keyring-daemon")
	}
	bus := dbusfixture.Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, "gnome-keyring-daemon", "--foreground", "--components=secrets", "--unlock", "--control-directory", filepath.Join(home, "control"))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_DATA_HOME=" + home, "DBUS_SESSION_BUS_ADDRESS=" + bus.Address}
	cmd.Stdin = strings.NewReader("synthetic-fixture-keyring-password\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	conn, err := bus.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A bounded readiness poll checks name ownership, never secret values.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var owned bool
		err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, secretName).Store(&owned)
		if err == nil && owned {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("isolated keyring did not start")
		case <-ticker.C:
		}
	}
	k := SecretService{connect: bus.Connect}
	account := strings.Repeat("a", 64)
	original, err := generateKeyset()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(original)
	if _, err = k.Load(ctx, account); !errors.Is(err, ErrMissing) {
		t.Fatal("missing", err)
	}
	created, err := k.CreateIfAbsent(ctx, account, original)
	if err != nil || !bytes.Equal(created, original) {
		t.Fatal("create", err)
	}
	existing, err := k.CreateIfAbsent(ctx, account, []byte("must not replace"))
	if err != nil || !bytes.Equal(existing, original) {
		t.Fatal("duplicate replaced key", err)
	}
	loaded, err := (SecretService{connect: bus.Connect}).Load(ctx, account)
	if err != nil || !bytes.Equal(loaded, original) {
		t.Fatal("fresh-session load", err)
	}
	if err = k.Delete(ctx, account); err != nil {
		t.Fatal("delete", err)
	}
	if err = k.Delete(ctx, account); err != nil {
		t.Fatal("idempotent delete", err)
	}
	if _, err = k.Load(ctx, account); !errors.Is(err, ErrMissing) {
		t.Fatal("purge", err)
	}
}
