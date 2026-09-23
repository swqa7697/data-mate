package vault

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
)

// Native access cannot be proven by the fake-provider scenarios. This one opt-in
// gate owns real Keychain restart, changed executable and launchd interoperability.
func TestNativeKeychainLifecycle(t *testing.T) {
	if os.Getenv("DATA_MATE_NATIVE_TEST") != "1" {
		t.Skip("requires DATA_MATE_NATIVE_TEST=1 and an unlocked macOS user Keychain/launchd session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	keys := Keychain{}
	dir, e := os.MkdirTemp("", "data-mate-p1-native-")
	if e != nil {
		t.Fatal(e)
	}
	nativeRoot := filepath.Join(dir, "root")
	if e := os.Mkdir(nativeRoot, 0700); e != nil {
		t.Fatal(e)
	}
	root, e := config.ResolveRoot(nativeRoot, "")
	if e != nil {
		t.Fatal(e)
	}

	binary := filepath.Join(dir, "native-helper")
	result := filepath.Join(dir, "result")
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("native %s failed: %v: %s", filepath.Base(name), e, out)
		}
	}
	metadata := filepath.Join(dir, "metadata")
	run("clang", "-x", "c", "-Wno-deprecated-declarations", "-framework", "Security", "-framework", "CoreFoundation", "testdata/native/metadata.c.txt", "-o", metadata)
	count := func(expected string) {
		t.Helper()
		out, e := exec.CommandContext(ctx, metadata, "count", root.Digest).CombinedOutput()
		if e != nil || strings.TrimSpace(string(out)) != expected {
			t.Fatalf("native exact item count: %v %s", e, out)
		}
	}
	count("0")
	boundaries := filepath.Join(dir, "boundaries")
	run("clang", "-x", "c", "-Wno-deprecated-declarations", "-framework", "Security", "-framework", "CoreFoundation", "testdata/native/boundaries.c.txt", "-o", boundaries)
	boundaryOut, boundaryErr := exec.CommandContext(ctx, boundaries, filepath.Join(dir, "isolated.keychain")).CombinedOutput()
	if boundaryErr != nil {
		t.Fatalf("native isolated boundaries: %v %s; retained %s", boundaryErr, boundaryOut, dir)
	}
	t.Logf("native isolated boundaries: %s", boundaryOut)
	run("go", "build", "-mod=readonly", "-o", binary, "./testdata/native")
	// Retain helper/root on cleanup failure so native approval can be retried.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		build := exec.CommandContext(cleanupCtx, "go", "build", "-mod=readonly", "-o", binary, "./testdata/native")
		if out, e := build.CombinedOutput(); e != nil {
			t.Errorf("cleanup helper rebuild: %v %s; retained %s", e, out, dir)
			return
		}
		cmd := exec.CommandContext(cleanupCtx, binary, "purge", root.Path, result)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Errorf("exact fixture cleanup: %v %s; retained %s", e, out, dir)
			return
		}
		if e := os.RemoveAll(dir); e != nil {
			t.Error(e)
		}
	})
	run(binary, "create", root.Path, result)
	count("1")
	run(binary, "verify", root.Path, result) // independent process and fresh key lookup
	// Copying a binary preserves its identity; rebuild with changed bytes instead.
	run("go", "build", "-mod=readonly", "-ldflags=-X main.buildIdentity=changed", "-o", binary, "./testdata/native")
	// A changed ad-hoc identity may require native approval. The unattended
	// probe accepts only explicit denial/interaction errors, and verifies the
	// original binary still decrypts the untouched bytes afterwards.
	changed := exec.CommandContext(ctx, binary, "verify", root.Path, result)
	out, changedErr := changed.CombinedOutput()

	var changedExit *exec.ExitError
	if changedErr != nil && (!errors.As(changedErr, &changedExit) || changedExit.ExitCode() != 3) {
		t.Fatalf("changed binary: %v %s", changedErr, out)
	}

	t.Logf("changed binary access: %v %s", changedErr, out)
	run("go", "build", "-mod=readonly", "-o", binary, "./testdata/native")
	run(binary, "verify", root.Path, result)
	label := "com.data-mate.p1-test." + root.Digest[:16]
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + label
	quote := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	plist := filepath.Join(dir, "fixture.plist")
	log := filepath.Join(dir, "launchd.log")
	body := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>` + quote(binary) + `</string><string>verify</string><string>` + quote(root.Path) + `</string><string>` + quote(result) + `</string></array><key>RunAtLoad</key><false/><key>KeepAlive</key><false/><key>StandardErrorPath</key><string>` + quote(log) + `</string></dict></plist>`
	if e := os.WriteFile(plist, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(result); e != nil {
		t.Fatal(e)
	}
	run("launchctl", "bootstrap", domain, plist)
	t.Cleanup(func() {
		cmd := exec.Command("launchctl", "bootout", target)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Errorf("owned launchd cleanup failed: %v %s", e, out)
		}
	})
	run("launchctl", "kickstart", target)
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		b, e := os.ReadFile(result)
		if e == nil && string(b) == "passed verify original\n" {
			break
		}
		select {
		case <-deadline.C:
			b, _ := os.ReadFile(log)
			t.Fatalf("native launchd result not ready: %s", b)
		case <-tick.C:
		}
	}
	run(binary, "purge", root.Path, result)
	count("0")
	if _, e := keys.Load(ctx, root.Digest); !errors.Is(e, ErrMissing) {
		t.Fatal("native exact-key purge", e)
	}
	t.Logf("native restart, changed-binary denial handling, launchd and exact purge passed; isolated digest %s", root.Digest)
}
