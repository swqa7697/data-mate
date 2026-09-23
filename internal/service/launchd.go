package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
)

func itoa(n int) string              { return strconv.Itoa(n) }
func domain() string                 { return "gui/" + itoa(os.Geteuid()) }
func target(root config.Root) string { return domain() + "/" + label(root) }

type job struct {
	Present bool
	PID     int
	Path    string
	Args    []string
}

// launchManager is the external process boundary; fakes cannot replace runtime
// ownership, socket verification, vault validation or state locking.
type launchManager interface {
	Inspect(context.Context, config.Root) (job, error)
	Bootstrap(context.Context, config.Root) error
	Bootout(context.Context, config.Root) error
}
type launchd struct{}
type boundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<10 {
		b.exceeded = true
		return 0, errors.New("launchctl output limit")
	}
	return b.Buffer.Write(p)
}
func launch(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME"), "LC_ALL=C"}
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if out.exceeded || ctx.Err() != nil {
		return "", ErrUnavailable
	}
	return out.String(), err
}
func (launchd) Inspect(ctx context.Context, root config.Root) (job, error) {
	out, err := launch(ctx, "print", target(root))
	if err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) && e.ExitCode() == 113 {
			return job{}, nil
		}
		return job{}, ErrUnavailable
	}
	// launchctl's human format is not a stable API. Accept only this observed
	// top-level structure and exact arguments; unknown layouts fail closed.
	j := job{Present: true}
	inArgs := false
	paths, argsBlocks, pids := 0, 0, 0
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || lines[0] != target(root)+" = {" || lines[len(lines)-1] != "}" {
		return job{}, ErrConflict
	}
	for _, line := range lines[1 : len(lines)-1] {
		if inArgs {
			if line == "\t}" {
				inArgs = false
				continue
			}
			if !strings.HasPrefix(line, "\t\t") || strings.HasPrefix(line, "\t\t\t") {
				return job{}, ErrConflict
			}
			j.Args = append(j.Args, strings.TrimPrefix(line, "\t\t"))
			continue
		}
		if strings.HasPrefix(line, "\tpath = ") {
			paths++
			j.Path = strings.TrimPrefix(line, "\tpath = ")
		}
		if line == "\targuments = {" {
			argsBlocks++
			inArgs = true
		}
		if strings.HasPrefix(line, "\tpid = ") {
			pids++
			j.PID, err = strconv.Atoi(strings.TrimPrefix(line, "\tpid = "))
			if err != nil || j.PID <= 0 {
				return job{}, ErrConflict
			}
		}
	}
	if paths != 1 || argsBlocks != 1 || pids > 1 || inArgs {
		return job{}, ErrConflict
	}
	return j, nil
}
func (launchd) Bootstrap(ctx context.Context, root config.Root) error {
	if _, err := launch(ctx, "bootstrap", domain(), filepath.Join(root.Path, "state/service.plist")); err != nil {
		return ErrStartup
	}
	return nil
}
func (launchd) Bootout(ctx context.Context, root config.Root) error {
	if _, err := launch(ctx, "bootout", target(root)); err != nil {
		return ErrUnavailable
	}
	return nil
}
func matching(j job, r record) bool {
	return j.Present && j.Path == filepath.Join(r.Identity.Root, "state/service.plist") && slices.Equal(j.Args, r.args())
}
func plist(root config.Root, r record) []byte {
	quote := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>` + label(root) + `</string><key>ProgramArguments</key><array>`)
	for _, a := range r.args() {
		b.WriteString("<string>" + quote(a) + "</string>")
	}
	// No login registration, socket activation, timers, or restart criteria.
	b.WriteString(`</array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/><key>ExitTimeOut</key><integer>5</integer><key>Umask</key><integer>63</integer><key>WorkingDirectory</key><string>/</string><key>StandardOutPath</key><string>/dev/null</string><key>StandardErrorPath</key><string>/dev/null</string></dict></plist>`)
	return []byte(b.String())
}
