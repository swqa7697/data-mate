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

	"github.com/godbus/dbus/v5"
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
	start := func() *exec.Cmd {
		t.Helper()
		args := []string{"--foreground", "--components=secrets", "--control-directory", control}
		cmd := exec.CommandContext(ctx, "gnome-keyring-daemon", args...)
		cmd.Env = env
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
	cmd := start()
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
	prompts := 0
	interactive := WithKeyringPrompt(WithInteraction(ctx, true), func(_ context.Context, c KeyringChallenge) ([]byte, error) {
		prompts++
		if c.Kind != "create" {
			t.Error("first setup did not request creation")
		}
		return []byte(password), nil
	})
	created, err := k.CreateIfAbsent(interactive, account, original)
	if err != nil || !bytes.Equal(created, original) {
		t.Fatal("create", err)
	}
	if prompts != 1 {
		t.Fatal("unexpected setup prompt count", prompts)
	}
	other := strings.Repeat("b", 64)
	if _, err = k.CreateIfAbsent(ctx, other, []byte("synthetic-unrelated-item")); err != nil {
		t.Fatal(err)
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
	start()
	if _, err = k.Load(ctx, account); !errors.Is(err, ErrLocked) {
		t.Fatal("restarted locked keyring", err)
	}
	// Exercise production terminal preparation, including a rejected password.
	prompts = 0
	interactive = WithKeyringPrompt(WithInteraction(ctx, true), func(_ context.Context, c KeyringChallenge) ([]byte, error) {
		prompts++
		if c.Kind != "unlock" || c.Attempt != prompts {
			t.Error("invalid retry challenge", c)
		}
		if prompts == 1 {
			return []byte("synthetic-wrong-password"), nil
		}
		return []byte(password), nil
	})
	loaded, err = k.Load(interactive, account)
	if err != nil || !bytes.Equal(loaded, original) || prompts != 2 {
		t.Fatal("terminal unlock/retry", err, prompts)
	}
	loaded, err = k.Load(ctx, account)
	if err != nil || !bytes.Equal(loaded, original) {
		t.Fatal("restarted unlocked keyring", err)
	}
	// Lock again and prepare deletion outside cleanup; the actual deletion
	// must work without any interactive callback and preserve other items.
	lock, err := k.open(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	var locked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	err = lock.call(ctx, secretRoot, secretInterface+"Service.Lock", 0, []dbus.ObjectPath{lock.collection}).Store(&locked, &prompt)
	lock.close()
	if err != nil {
		t.Fatal(err)
	}
	interactive = WithKeyringPrompt(WithInteraction(ctx, true), func(context.Context, KeyringChallenge) ([]byte, error) { return []byte(password), nil })
	if err = k.PrepareDelete(interactive, account); err != nil {
		t.Fatal("prepare purge", err)
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
	preserved, err := k.Load(ctx, other)
	if err != nil || string(preserved) != "synthetic-unrelated-item" {
		t.Fatal("purge damaged existing item", err)
	}
	// The fixture's keyring is persistent and password protected after restart;
	// no mutable plaintext password or keyset may be written by Data Mate.
	for _, secret := range [][]byte{[]byte(password), original} {
		err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				raw, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				if bytes.Contains(raw, secret) {
					t.Error("plaintext in fixture file", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
