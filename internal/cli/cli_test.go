package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {
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
		{name: "upgrade remains informational", args: []string{"upgrade"}},
		{name: "update remains informational", args: []string{"update"}},
		{name: "unfinished db fails", args: []string{"db", "list"}, code: 1},
		{name: "unfinished bridge keeps stdout empty", args: []string{"mcp", "bridge"}, code: 1},
		{name: "relative root rejected", args: []string{"db", "list", "--root", "relative"}, code: 2},
		{name: "password flag redacted", args: []string{"--password", "secret-sentinel"}, code: 2},
		{name: "unknown command redacted", args: []string{"secret-sentinel"}, code: 2},
		{name: "unexpected argument redacted", args: []string{"version", "secret-sentinel"}, code: 2},
		{name: "cancellation leaves state untouched", args: []string{"db", "list"}, code: 130, cancelled: true},
		{name: "output failure propagates", args: []string{"version"}, code: 1, outputFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
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
