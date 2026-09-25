// Command release implements checkout-only release maintenance, never runtime commands.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"
)

type app struct {
	ctx      context.Context
	root     string
	in       io.Reader
	out      io.Writer
	terminal bool
	code     func() (string, error)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	a := &app{ctx: ctx, in: os.Stdin, out: os.Stdout, terminal: term.IsTerminal(int(os.Stdin.Fd())), code: captchaCode}
	root, err := a.git("rev-parse", "--show-toplevel")
	if err == nil {
		a.root = strings.TrimSpace(root)
		err = a.execute(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func (a *app) execute(args []string) error {
	if len(args) == 2 && args[0] == "bump" {
		return a.bump(args[1], time.Now().Format("2006-01-02"))
	}
	if len(args) == 1 && args[0] == "commit" {
		return a.commit(false)
	}
	if len(args) == 2 && args[0] == "commit" && args[1] == "--yes" {
		return a.commit(true)
	}
	if len(args) == 1 && args[0] == "tag" {
		return a.tag()
	}
	if len(args) == 2 && args[0] == "validate" {
		_, err := a.validate(args[1])
		return err
	}
	if len(args) == 2 && args[0] == "notes" {
		version, err := parseVersion(args[1])
		if err != nil {
			return err
		}
		raw, err := a.read("CHANGELOG.md")
		if err != nil {
			return err
		}
		notes, err := section(raw, version, true)
		if err == nil {
			_, err = fmt.Fprintln(a.out, notes)
		}
		return err
	}
	if len(args) == 3 && args[0] == "publish" {
		return a.publish(args[1], args[2])
	}
	return errors.New("usage: release {bump major|minor|patch | commit [--yes] | tag | validate TAG | notes VERSION | publish TAG ARTIFACT_DIRECTORY}")
}

// Each child has a deadline, including git authentication and upload failures.
func (a *app) command(input string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = a.root
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (a *app) git(args ...string) (string, error) { return a.command("", "git", args...) }

func (a *app) confirm(prompt string) (string, error) {
	if !a.terminal {
		return "", errors.New("confirmation requires an interactive terminal")
	}
	fmt.Fprint(a.out, prompt)
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("confirmation aborted: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func captchaCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n), nil
}

func (a *app) captcha() error {
	if !a.terminal {
		return errors.New("creating a tag requires an interactive terminal")
	}
	code, err := a.code()
	if err != nil {
		return err
	}
	font := [10][5]string{
		{"███", "█ █", "█ █", "█ █", "███"}, {" █ ", "██ ", " █ ", " █ ", "███"},
		{"███", "  █", "███", "█  ", "███"}, {"███", "  █", "███", "  █", "███"},
		{"█ █", "█ █", "███", "  █", "  █"}, {"███", "█  ", "███", "  █", "███"},
		{"███", "█  ", "███", "█ █", "███"}, {"███", "  █", "  █", "  █", "  █"},
		{"███", "█ █", "███", "█ █", "███"}, {"███", "█ █", "███", "  █", "███"},
	}
	for row := range 5 {
		for _, digit := range code {
			fmt.Fprint(a.out, "   ", font[digit-'0'][row])
		}
		fmt.Fprintln(a.out)
	}
	answer, err := a.confirm("Type the four digits above to push the tag and start publication: ")
	if err != nil {
		return err
	}
	if answer != code {
		return errors.New("CAPTCHA mismatch; aborted")
	}
	return nil
}
