package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Environment is selected by immutable build metadata, never user configuration.
type Environment string

const (
	Development Environment = "development"
	Production  Environment = "production"
)

// Kind normalizes the ordinary source-build default.
func (e Environment) Kind() Environment {
	if e == "" {
		return Development
	}
	return e
}

// AccountHome ignores HOME and other process environment overrides.
func AccountHome() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// ProductionRoot resolves a fixed path passively, including a fresh installation.
// The explicit home argument is an injection seam, not a runtime root option.
func ProductionRoot(home string) (Root, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || strings.ContainsAny(home, "\x00\r\n\t") {
		return Root{}, ErrOwnership
	}
	path := filepath.Join(home, ".local", "share", "data-mate")
	if err := CheckPath(path); err != nil {
		return Root{}, err
	}
	sum := sha256.Sum256([]byte(path))
	return Root{Path: path, Digest: hex.EncodeToString(sum[:]), Environment: Production}, nil
}

// CheckPath rejects symlinks and writable/unowned directory ancestors. Missing
// suffixes are allowed for passive discovery; callers must recheck before writes.
func CheckPath(path string) error {
	if !filepath.IsAbs(path) {
		return ErrOwnership
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ErrOwnership
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || (raw.Uid != uint32(os.Geteuid()) && raw.Uid != 0) || (st.Mode().Perm()&0022 != 0 && !(raw.Uid == 0 && st.Mode()&os.ModeSticky != 0)) {
			return ErrOwnership
		}
	}
	return nil
}

// ExecutablePath is the environment's installation target, not cleanup authority.
func ExecutablePath(root Root) string {
	if root.Environment.Kind() == Production {
		return filepath.Join(root.Path, "bin", "data-mate")
	}
	return DevelopmentExecutable(root)
}

func validateRoot(root Root) error {
	actual, err := ResolveRoot(root.Path, "")
	if err != nil || actual.Path != root.Path || actual.Digest != root.Digest {
		return ErrOwnership
	}
	if root.Environment.Kind() != Development && root.Environment.Kind() != Production {
		return ErrOwnership
	}
	return nil
}
