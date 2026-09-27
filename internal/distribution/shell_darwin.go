package distribution

import (
	"context"
	"os/user"
	"path/filepath"
	"strings"
)

// LoginShell reads the OS account's login shell without trusting SHELL or HOME.
func LoginShell(ctx context.Context) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	raw, err := command(ctx, "/usr/bin/dscl", ".", "-read", "/Users/"+u.Username, "UserShell")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || fields[0] != "UserShell:" {
		return "", ErrConflict
	}
	return filepath.Base(fields[1]), nil
}
