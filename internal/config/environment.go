package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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

// CheckPath checks syntax only. User-managed directory permissions and symlinks
// are handled by normal filesystem operations, not installation policy.
func CheckPath(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\t") {
		return ErrOwnership
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
	sum := sha256.Sum256([]byte(root.Path))
	if !filepath.IsAbs(root.Path) || filepath.Clean(root.Path) != root.Path || hex.EncodeToString(sum[:]) != root.Digest {
		return ErrOwnership
	}
	if root.Environment.Kind() != Development && root.Environment.Kind() != Production {
		return ErrOwnership
	}
	return nil
}
