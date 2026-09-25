// Package service owns the background process, admission and installation lifecycle.
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

var (
	ErrState       = errors.New("invalid or unsafe service state")
	ErrUnavailable = errors.New("service unavailable; run mcp start")
	ErrConflict    = errors.New("service identity conflict; owned state was preserved")
	ErrRestart     = errors.New("service build or protocol changed; run mcp stop then mcp start")
	ErrStartup     = errors.New("service failed readiness; check profiles and native Keychain access")
)

// Build identifies the application and executable bytes, including dirty rebuilds.
type Build struct {
	Version     string `json:"version"`
	Revision    string `json:"revision"`
	Fingerprint string `json:"fingerprint"`
}

// ExecutableBuild hashes the running executable before lifecycle work.
func ExecutableBuild(version, revision string) (Build, error) {
	p, err := os.Executable()
	if err != nil {
		return Build{}, ErrState
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return Build{}, ErrState
	}
	hash, err := executableHash(p, false)
	return Build{version, revision, hash}, err
}
func binaryHash(path string) (string, error) { return executableHash(path, true) }
func executableHash(path string, private bool) (string, error) {
	var fd int
	var err error
	if private {
		parent, e := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return "", ErrState
		}
		defer unix.Close(parent)
		var st unix.Stat_t
		if unix.Fstat(parent, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700 {
			return "", ErrState
		}
		fd, err = unix.Openat(parent, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	} else {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return "", ErrState
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || (st.Mode&07777 != 0700 && (private || st.Mode&07777 != 0755)) || st.Nlink != 1 {
		return "", ErrState
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", ErrState
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type identity struct {
	Installation string             `json:"installation"`
	Root         string             `json:"root"`
	Digest       string             `json:"digest"`
	Environment  config.Environment `json:"environment,omitempty"`
}

func installation(root config.Root, id config.Identity) identity {
	return identity{id.ID, root.Path, root.Digest, root.Environment.Kind()}
}

type record struct {
	Protocol   int      `json:"protocol"`
	Identity   identity `json:"identity"`
	Build      Build    `json:"build"`
	Nonce      string   `json:"nonce"`
	PID        int      `json:"pid"`
	Executable string   `json:"executable"`
}

func (r record) args() []string {
	args := []string{r.Executable, "__service", "--instance", r.Nonce}
	if r.Identity.Environment.Kind() == config.Development {
		args = append(args, "--root", r.Identity.Root)
	}
	return args
}
func label(root config.Root) string { return "com.data-mate.service" }
func socketDir(root config.Root) string {
	return "/private/tmp/dm-" + itoa(os.Geteuid()) + "-" + root.Digest[:16]
}
func readRecord(read func(string, int) ([]byte, error), root config.Root, id config.Identity) (record, error) {
	b, err := read("service.json", 4096)
	if err != nil {
		return record{}, err
	}
	var r record
	if config.DecodeStrict(b, 4096, &r) != nil || r.Protocol != 3 || r.Executable == "" || r.Executable != id.Executable || r.Identity != installation(root, id) || !config.ValidUUID(r.Nonce) || r.PID < 0 || r.Build.Version == "" || r.Build.Revision == "" || len(r.Build.Fingerprint) != 64 {
		return record{}, ErrState
	}
	return r, nil
}
func saveRecord(l *config.LifecycleLease, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return ErrState
	}
	return l.Replace("service.json", b)
}
func safePath(root config.Root) bool { return !strings.ContainsAny(root.Path, "\n\r\t\x00") }
