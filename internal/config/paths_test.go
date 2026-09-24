package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoots(t *testing.T) {
	parent := t.TempDir()
	var last Root
	for _, name := range []string{"first checkout", "second checkout"} {
		path := filepath.Join(parent, name, ".dev", "data-mate")
		bin := filepath.Join(filepath.Dir(path), "bin", "data-mate")
		if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, nil, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		root, err := ResolveRoot("", bin)
		if err != nil {
			t.Fatal(err)
		}
		explicit, err := ResolveRoot(path, "ignored")
		if err != nil || explicit != root {
			t.Fatal("override")
		}
		if len(root.Digest) != 64 || root.Digest == last.Digest {
			t.Fatal("root namespaces collide")
		}
		last = root
		link := filepath.Join(parent, name, "link")
		if err := os.Symlink(bin, link); err != nil {
			t.Fatal(err)
		}
		linked, err := ResolveRoot("", link)
		if err != nil || linked != root {
			t.Fatal("symlink executable")
		}
	}
	if _, err := ResolveRoot("relative", "ignored"); err == nil {
		t.Fatal("relative root accepted")
	}
	if _, err := ResolveRoot(filepath.Join(parent, "missing"), ""); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := ResolveRoot("", os.Args[0]); err == nil {
		t.Fatal("uninstalled executable inferred root")
	}
}
