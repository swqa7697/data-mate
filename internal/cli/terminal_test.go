package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
)

// Real PTYs are necessary for hidden input, raw-mode restoration and cancellation;
// the existing CLI tests use buffers and cannot exercise terminal behavior.
func TestConnectionTerminal(t *testing.T) {
	if mode := os.Getenv("DATA_MATE_P2_PTY_HELPER"); mode != "" {
		root := os.Getenv("DATA_MATE_P2_PTY_ROOT")
		keys := &testKeys{}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		fixture := &fixtureDatabase{browseError: mode == "scope-fail", bulkError: mode == "scope-bulk-fail", bulkWait: mode == "scope-bulk-cancel"}
		if mode == "scope-limit" {
			fixture.schemaCount = 5000
		}
		factory := databaseFactory(defaultDatabase)
		if strings.HasPrefix(mode, "scope") || mode == "diagnostics" {
			factory = func() (cliDatabase, error) { return fixture, nil }
		}
		run := func(args ...string) int {
			cmd := commandWithDatabase(Build{}, keys, factory)
			cmd.SetArgs(append([]string{"--root", root, "db"}, args...))
			cmd.SetIn(os.Stdin)
			cmd.SetOut(os.Stdout)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return ExitCode(err)
		}
		// Extend the PTY harness because buffered diagnostics cannot verify output
		// terminal detection, especially when stdin is redirected.
		if mode == "diagnostics" {
			for _, alias := range []string{"good", "bad"} {
				command(t, root, keys, "", 0, "add", "--alias", alias, "--host", "localhost", "--database", "app", "--username", "reader", "--passwordless", "--yes")
			}
			for _, output := range []string{"color", "no-color", "empty-no-color", "json"} {
				switch output {
				case "no-color":
					t.Setenv("NO_COLOR", "1")
				case "empty-no-color":
					t.Setenv("NO_COLOR", "")
				default:
					if err := os.Unsetenv("NO_COLOR"); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"test"}
				if output == "json" {
					args = append(args, "--json")
				}
				fmt.Println("diagnostics " + output)
				if code := run(args...); code != ExitFailure {
					t.Fatalf("diagnostics %s exit: %d", output, code)
				}
				fmt.Println("diagnostics end")
			}
			os.Exit(0)
		}
		if strings.HasPrefix(mode, "scope") {
			args := append(append([]string{}, basicAdd...), "--passwordless")
			if mode == "scope-mixed" {
				args = append(args, "--exclude-schema", "missing")
			}
			if code := run(args...); code != 0 {
				os.Exit(code)
			}
			before := files(t, root)
			code := run("scope", "analytics")
			if mode == "scope-mixed" {
				want := config.Scope{Mode: "blacklist", Schemas: []string{"Dot.Schema", "missing", "schema0049"}}
				if got := snapshot(t, root).Connections[0].Scope; code != 0 || !reflect.DeepEqual(got, want) {
					t.Fatalf("mode switches lost off-page or missing names: %+v", got)
				}
				code = run("scope", "analytics")
			}
			if mode == "scope-mixed" || mode == "scope-color" || mode == "scope-limit" {
				p := snapshot(t, root).Connections[0]
				want := config.Scope{Mode: "blacklist", Schemas: []string{"Dot.Schema"}}
				if mode == "scope-mixed" {
					want.Mode = "whitelist"
				}
				if code != 0 || !reflect.DeepEqual(p.Scope, want) || !fixture.closed {
					t.Fatalf("schema picker: %+v requests=%+v code=%d", p.Scope, fixture.requests, code)
				}
				if mode == "scope-mixed" {
					for _, selection := range []string{"all", "none"} {
						if code = run("scope"); code != 0 {
							os.Exit(code)
						}
						scope := snapshot(t, root).Connections[0].Scope
						if (selection == "all") != scope.ContainsSchema("future") || len(scope.Schemas) != 0 {
							t.Fatal("interactive all/none failed")
						}
					}
				}
			} else if !reflect.DeepEqual(before, files(t, root)) {
				t.Fatal("failed/canceled picker modified state")
			}
			os.Exit(code)
		}
		if strings.HasPrefix(mode, "enroll") {
			peer := transportfixture.New(t, "ssh", transportfixture.Options{User: "fixture", Password: "synthetic"})
			args := []string{"add", "--alias", "analytics", "--host", "localhost", "--database", "app", "--username", "reader", "--passwordless", "--ssh-host", "127.0.0.1", "--ssh-port", strconv.Itoa(peer.SSHConfig("password").Port), "--ssh-user", "fixture", "--ssh-enroll"}
			code := run(args...)
			peer.Close()
			if mode == "enroll" && code == 0 {
				p := snapshot(t, root)
				if credential(t, root, keys, p.Connections[0].ID).SSHPassword != "synthetic" {
					t.Fatal("SSH password not encrypted")
				}
			}
			os.Exit(code)
		}
		code := run("add")
		if mode != "happy" {
			os.Exit(code)
		}
		if code != 0 {
			os.Exit(code)
		}
		p := snapshot(t, root)
		id := p.Connections[0].ID
		if credential(t, root, keys, id).Password != "pty-hidden-secret" {
			t.Fatal("terminal secret changed")
		}
		before, calls := files(t, root), keys.calls
		if code = run("edit", "analytics", "--alias", "cancelled"); code != ExitCancelled {
			t.Fatal("negative edit confirmation not cancelled")
		}
		if !reflect.DeepEqual(before, files(t, root)) || keys.calls != calls {
			t.Fatal("cancelled edit changed files or accessed keys")
		}
		if code = run("edit"); code != 0 {
			os.Exit(code)
		}
		p = snapshot(t, root)
		if p.Connections[0].ID != id || p.Connections[0].Alias != "renamed" || credential(t, root, keys, id).Password != "pty-hidden-secret" {
			t.Fatal("terminal edit did not preserve ID/password")
		}
		if code = run("rm"); code != 0 {
			os.Exit(code)
		}
		if len(snapshot(t, root).Connections) != 0 {
			t.Fatal("terminal removal failed")
		}
		os.Exit(run("ls", "--json"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "testdata/terminal.py", os.Args[0], t.TempDir())
	cmd.WaitDelay = time.Second
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY regression: %v\n%s", err, b)
	}
}
