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
	control := filepath.Join(home, "control")
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_DATA_HOME=" + home, "DBUS_SESSION_BUS_ADDRESS=" + bus.Address}
	const password = "synthetic-fixture-keyring-password"
	conn, err := bus.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := func(unlock bool) *exec.Cmd {
		t.Helper()
		args := []string{"--foreground", "--components=secrets", "--control-directory", control}
		if unlock {
			args = append(args, "--unlock")
		}
		cmd := exec.CommandContext(ctx, "gnome-keyring-daemon", args...)
		cmd.Env = env
		if unlock {
			cmd.Stdin = strings.NewReader(password)
		}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		// Check the new process owns the name, including after a restart.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var pid uint32
			err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, secretName).Store(&pid)
			if err == nil && pid == uint32(cmd.Process.Pid) {
				return cmd
			}
			select {
			case <-ctx.Done():
				t.Fatal("isolated keyring did not start")
			case <-ticker.C:
			}
		}
	}
	cmd := start(true)
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
	// A restarted GNOME daemon exposes hashed attributes while locked and adds
	// xdg:schema on reload. Neither may turn a valid keyset into ErrUnavailable.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	start(false)
	if _, err = k.Load(ctx, account); !errors.Is(err, ErrLocked) {
		t.Fatal("restarted locked keyring", err)
	}
	// GNOME's --unlock starts a daemon; it does not unlock the existing one.
	// Use the fixture provider's private API to unlock its synthetic collection.
	unlock, err := k.open(ctx, account)
	if err != nil {
		t.Fatal("open fixture unlock session", err)
	}
	err = unlock.object(secretRoot).CallWithContext(ctx, "org.gnome.keyring.InternalUnsupportedGuiltRiddenInterface.UnlockWithMasterPassword", 0,
		unlock.collection, busSecret{unlock.session, []byte{}, []byte(password), "text/plain"}).Err
	unlock.close()
	if err != nil {
		t.Fatal("unlock restarted keyring", err)
	}
	loaded, err = k.Load(ctx, account)
	if err != nil || !bytes.Equal(loaded, original) {
		t.Fatal("restarted unlocked keyring", err)
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
