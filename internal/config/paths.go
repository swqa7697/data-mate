package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// Root identifies a stable absolute data path without creating state.
// User-managed data directory links retain their logical path and namespace.
type Root struct {
	Path        string
	Digest      string
	Environment Environment
}

// ResolveRoot uses an absolute override or an installed bin/data-mate executable.
// Executable symlinks resolve to the installation, never the caller's directory.
func ResolveRoot(override, executable string) (Root, error) {
	path := override
	if path != "" {
		if !filepath.IsAbs(path) {
			return Root{}, errors.New("root must be absolute")
		}
	} else {
		real, err := filepath.EvalSymlinks(executable)
		if err != nil {
			return Root{}, errors.New("cannot resolve installed executable")
		}
		if filepath.Base(real) != "data-mate" || filepath.Base(filepath.Dir(real)) != "bin" {
			return Root{}, errors.New("use installed bin/data-mate or an absolute --root")
		}
		path = filepath.Join(filepath.Dir(filepath.Dir(real)), "data-mate")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	real := filepath.Join(parent, filepath.Base(path))
	if err != nil {
		return Root{}, errors.New("cannot resolve installation root")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return Root{}, errors.New("root must be an existing directory")
	}
	sum := sha256.Sum256([]byte(real))
	return Root{Path: real, Digest: hex.EncodeToString(sum[:])}, nil
}
