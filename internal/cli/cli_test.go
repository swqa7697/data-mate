package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"help"}, 0, "version"}, {[]string{"help", "db"}, 0, "Manage database"}, {[]string{"help", "secret-sentinel"}, 2, "invalid help topic"}, {[]string{"db", "secret-sentinel"}, 2, "invalid command"}, {[]string{"mcp", "secret-sentinel"}, 2, "invalid command"}, {[]string{"version"}, 0, "data-mate test-version"},
		{[]string{"upgrade"}, 0, "make install"}, {[]string{"update"}, 0, "make install"},
		{[]string{"db", "list", "--root", root}, 1, "SERVICE_UNAVAILABLE"},
		{[]string{"mcp", "bridge", "--root", root}, 1, "SERVICE_UNAVAILABLE"},
		{[]string{"db", "list", "--root", "relative"}, 2, "root must be absolute"},
		{[]string{"--password", "secret-sentinel"}, 2, "invalid flags"},
		{[]string{"secret-sentinel"}, 2, "invalid command"}, {[]string{"version", "secret-sentinel"}, 2, "invalid command"},
	} {
		t.Run(strings.Join(tc.args[:1], " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, &out, &stderr, Build{Version: "test-version", Revision: "test", Dirty: "false"})
			text := out.String() + stderr.String()
			if code != tc.code || !strings.Contains(text, tc.want) {
				t.Fatalf("code=%d output=%s", code, text)
			}
			if strings.Contains(text, "secret-sentinel") {
				t.Fatal("input leaked")
			}
			if tc.code != 0 && out.Len() != 0 {
				t.Fatal("error on stdout")
			}
		})
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("CLI changed installation")
	}
}
func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{nil, 0}, {errors.New("operational"), 1}, {&Error{ExitInvalid, "invalid"}, 2}, {context.Canceled, 130}} {
		if ExitCode(tc.err) != tc.code {
			t.Fatal(tc)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if Run(ctx, []string{"db", "list"}, &out, &out, Build{}) != 130 {
		t.Fatal("cancellation")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }
func TestOutputFailure(t *testing.T) {
	var diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, failingWriter{}, &diagnostic, Build{}); code != ExitFailure {
		t.Fatalf("got exit %d", code)
	}
}
