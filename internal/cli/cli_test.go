package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/swqa7697/data-mate/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {

	// Production's rejected root flag must not reach filesystem or network seams,
	// even on otherwise passive/help and hidden candidate/helper entry points.
	t.Run("production rejects root before effects", func(t *testing.T) {
		for _, command := range [][]string{{"version"}, {"help"}, {"upgrade"}, {"uninstall", "--yes"}, {"completion", "bash"}, {"__install"}, {"__cleanup"}, {"__service"}, {"__release-metadata"}, {"__complete", "db", "edit", ""}} {
			for _, flags := range [][]string{{"--root", "secret-sentinel"}, {"--root=secret-sentinel"}} {
				cmd := New(Build{Environment: config.Production, accountHome: func() (string, error) { t.Fatal("rejected command resolved account home"); return "", nil }})
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs(append(append([]string{}, command...), flags...))
				if err := cmd.ExecuteContext(t.Context()); err == nil || ExitCode(err) != 2 || strings.Contains(out.String(), "secret-sentinel") {
					t.Fatal(command, flags, err, out.String())
				}
			}
		}
	})
	t.Run("production completion is passive before install", func(t *testing.T) {
		home := t.TempDir()
		cmd := New(Build{Environment: config.Production, accountHome: func() (string, error) { return home, nil }})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"__complete", "db", "edit", ""})
		if err := cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
			t.Fatal("completion initialized production", err)
		}
	})
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		name          string
		args          []string
		code          int
		cancelled     bool
		outputFailure bool
	}{
		{name: "help writes stdout", args: []string{"help"}},
		{name: "nested help writes stdout", args: []string{"help", "db"}},
		{name: "invalid help redacts topic", args: []string{"help", "secret-sentinel"}, code: 2},
		{name: "db rejects unknown command", args: []string{"db", "secret-sentinel"}, code: 2},
		{name: "mcp rejects unknown command", args: []string{"mcp", "secret-sentinel"}, code: 2},
		{name: "version writes stdout", args: []string{"version"}},
		{name: "development upgrade requires make", args: []string{"upgrade"}, code: 2},
		{name: "development update requires make", args: []string{"update"}, code: 2},
		{name: "development purge requires make", args: []string{"uninstall", "--purge", "--yes"}, code: 2},
		{name: "empty list is read only", args: []string{"db", "list"}},
		{name: "stopped status is passive", args: []string{"mcp", "status", "--json"}},
		{name: "stopped stop is idempotent", args: []string{"mcp", "stop"}},
		{name: "stopped bridge keeps stdout empty", args: []string{"mcp", "bridge"}, code: 1},
		{name: "relative root rejected", args: []string{"db", "list", "--root", "relative"}, code: 2},
		{name: "password flag redacted", args: []string{"--password", "secret-sentinel"}, code: 2},
		{name: "unknown command redacted", args: []string{"secret-sentinel"}, code: 2},
		{name: "unexpected argument redacted", args: []string{"version", "secret-sentinel"}, code: 2},
		{name: "cancellation leaves state untouched", args: []string{"db", "list"}, code: 130, cancelled: true},
		{name: "output failure propagates", args: []string{"version"}, code: 1, outputFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--root", root}, tc.args...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			var out, stderr bytes.Buffer
			var stdout io.Writer = &out
			if tc.outputFailure {
				stdout = failingWriter{}
			}
			code := Run(ctx, args, stdout, &stderr, Build{Version: "test-version", Revision: "test", Dirty: "false"})
			if code != tc.code {
				t.Fatalf("code=%d want=%d stdout=%q stderr=%q", code, tc.code, out.String(), stderr.String())
			}
			if strings.Contains(out.String()+stderr.String(), "secret-sentinel") {
				t.Fatal("input leaked")
			}
			if tc.code == 0 {
				if out.Len() == 0 || stderr.Len() != 0 {
					t.Fatal("success must write stdout only")
				}
			} else if out.Len() != 0 || stderr.Len() == 0 {
				t.Fatal("failure must write stderr only")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("CLI changed installation", err)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }
