package devtools_test

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, dir string, wantSuccess bool, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// Cancel the entire make/go process group so a timeout cannot leak builders.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	b, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%v: %v\n%s", args, ctx.Err(), b)
	}
	if (err == nil) != wantSuccess {
		t.Fatalf("%v: %v\n%s", args, err, b)
	}
	return string(b)
}
func copyCheckout(t *testing.T, dest string) {
	t.Helper()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"cmd", "internal", "scripts", "Makefile", "VERSION", "go.mod", "go.sum"} {
		err := filepath.WalkDir(filepath.Join(source, path), func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(source, name)
			if err != nil {
				return err
			}
			target := filepath.Join(dest, rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, info.Mode().Perm())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
