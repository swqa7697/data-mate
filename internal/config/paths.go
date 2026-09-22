package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// Root identifies a canonical development installation without creating state.
type Root struct {
	Path   string
	Digest string
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
		path = filepath.Dir(filepath.Dir(real))
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Root{}, errors.New("cannot resolve installation root")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return Root{}, errors.New("root must be an existing directory")
	}
	sum := sha256.Sum256([]byte(real))
	return Root{real, hex.EncodeToString(sum[:])}, nil
}
