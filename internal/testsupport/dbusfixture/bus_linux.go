// Package dbusfixture owns a private message bus, never the user's desktop bus.
package dbusfixture

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/godbus/dbus/v5"
)

type Bus struct{ Address string }

func Start(t *testing.T) *Bus {
	t.Helper()
	// Do not load host service directories: every peer belongs to the fixture,
	// and installed desktop services must not mask activation failures in CI.
	config := filepath.Join(t.TempDir(), "bus.conf")
	if err := os.WriteFile(config, []byte(`<busconfig>
  <type>session</type>
  <listen>unix:tmpdir=/tmp</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*"/>
    <allow receive_sender="*"/>
    <allow own="*"/>
  </policy>
</busconfig>`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--config-file="+config, "--nofork", "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatalf("private bus requires dbus-daemon: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	reader := bufio.NewReader(io.LimitReader(stdout, 4096))
	address, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return &Bus{Address: address[:len(address)-1]}
}
func (b *Bus) Connect(ctx context.Context) (*dbus.Conn, error) {
	return dbus.Connect(b.Address, dbus.WithContext(ctx))
}
