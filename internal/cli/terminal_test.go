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

	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/testsupport/transportfixture"
	"golang.org/x/sys/unix"
)

// Real PTYs are necessary for hidden input, raw-mode restoration and cancellation;
// the existing CLI tests use buffers and cannot exercise terminal behavior.
func TestConnectionTerminal(t *testing.T) {
	if mode := os.Getenv("DATA_MATE_P2_PTY_HELPER"); mode != "" {
		root := os.Getenv("DATA_MATE_P2_PTY_ROOT")
		keys := &testKeys{}
		if strings.HasPrefix(mode, "keyring") {
			keys.keyring = "create"
			if mode == "keyring-unlock" {
				keys.keyring = "unlock"
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		fixture := &fixtureDatabase{}
		factory := databaseFactory(defaultDatabase)
		if strings.HasPrefix(mode, "describe") || strings.HasPrefix(mode, "catalog") || mode == "diagnostics" {
			factory = func() (cliDatabase, error) { return fixture, nil }
		}
		run := func(args ...string) int {
			cmd := commandWithDatabase(Build{}, keys, factory)
			cmd.SetArgs(append([]string{"--root", root, "db"}, args...))
			cmd.SetIn(os.Stdin)
			if mode == "keyring-noninteractive" {
				cmd.SetIn(strings.NewReader("pty-hidden-secret"))
			}
			cmd.SetOut(os.Stdout)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return ExitCode(err)
		}
		// Extend the same PTY scenario: paging must release the service first,
		// honor output width/colors, exit on q, and restore modes on cancellation.
		if strings.HasPrefix(mode, "catalog") {
			command(t, root, keys, "", 0, append(basicAdd, "--passwordless")...)
			schema := database.SchemaDescription{Name: "public", Tables: []database.RelationName{}, Enums: []database.CatalogName{{Name: "status"}}, Sequences: []database.CatalogName{{Name: "items_id_seq"}}, Indexes: []database.CatalogName{{Name: "alpha_idx"}}, Functions: []database.CatalogName{{Name: "lookup"}}}
			for _, name := range []string{"alpha", "beta", "delta", "gamma"} {
				schema.Tables = append(schema.Tables, database.RelationName{Name: name, Kind: "table"})
			}
			schema.Tables = append(schema.Tables, database.RelationName{Name: "report", Kind: "view"})
			fixture.catalog = []database.SchemaDescription{schema}
			if mode == "catalog-layout" {
				for _, variant := range []string{"color", "narrow", "no-color", "empty-no-color", "json"} {
					columns := uint16(80)
					if variant == "narrow" {
						columns = 12
					}
					if err := unix.IoctlSetWinsize(int(os.Stdout.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: columns}); err != nil {
						t.Fatal(err)
					}
					if err := os.Unsetenv("NO_COLOR"); err != nil {
						t.Fatal(err)
					}
					if variant == "no-color" {
						t.Setenv("NO_COLOR", "1")
					}
					if variant == "empty-no-color" {
						t.Setenv("NO_COLOR", "")
					}
					args := []string{"describe", "analytics", "--no-pager"}
					if variant == "json" {
						args = append(args, "--json")
					}
					fmt.Println("catalog " + variant)
					if code := run(args...); code != 0 {
						t.Fatalf("catalog output: %d", code)
					}
					fmt.Println("catalog end")
				}
				os.Exit(0)
			}
			if mode != "catalog-short" {
				fixture.catalog[0].Tables = nil
				for i := 0; i < 1500; i++ {
					fixture.catalog[0].Tables = append(fixture.catalog[0].Tables, database.RelationName{Name: fmt.Sprintf("item_%04d_%s", i, strings.Repeat("x", 40)), Kind: "table"})
				}
			}
			args := []string{"describe", "analytics"}
			if mode == "catalog-no-pager" {
				args = append(args, "--no-pager")
			}
			if mode == "catalog-json" {
				args = append(args, "--json")
			}
			code := run(args...)
			if !fixture.closed {
				t.Fatal("pager retained management resources")
			}
			fmt.Println("catalog done")
			os.Exit(code)
		}
		// Extend the PTY harness because buffered diagnostics cannot verify output
		// terminal detection, especially when stdin is redirected.
		if strings.HasPrefix(mode, "describe") {
			command(t, root, keys, "", 0, append(basicAdd, "--passwordless")...)
			before := files(t, root)
			code := run("describe", "--json")
			if !reflect.DeepEqual(before, files(t, root)) {
				t.Fatal("description changed saved state")
			}
			if mode == "describe" && (code != 0 || len(fixture.described) != 1) {
				t.Fatal("description picker did not select profile")
			}
			if mode == "describe-cancel" && (code != ExitCancelled || len(fixture.described) != 0) {
				t.Fatal("canceled description reached database")
			}
			os.Exit(code)
		}
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
		if mode == "keyring-noninteractive" {
			code := run(append(basicAdd, "--password-stdin")...)
			if code != ExitFailure {
				t.Fatal("noninteractive keyring authentication", code)
			}
			if len(snapshot(t, root).Connections) != 0 {
				t.Fatal("noninteractive request published")
			}
			os.Exit(code)
		}
		code := run("add")
		if strings.HasPrefix(mode, "keyring") {
			keys.keyring = ""
			p := snapshot(t, root)
			if mode == "keyring-cancel" {
				if code != ExitCancelled || len(p.Connections) != 0 {
					t.Fatal("canceled keyring request published", code)
				}
			} else {
				if code != 0 || len(p.Connections) != 1 || credential(t, root, keys, p.Connections[0].ID).Password != "pty-hidden-secret" {
					t.Fatal("keyring preparation did not resume original save", code)
				}
			}
			os.Exit(code)
		}
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
