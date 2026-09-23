package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// PreviewKnownHosts reads nonsecret owned host pins without initializing state.
// A confirmed writer must recheck pins under its exclusive state lease.
func PreviewKnownHosts(ctx context.Context, root Root) ([]byte, error) {
	if _, _, err := Preview(ctx, root); err != nil {
		return nil, err
	}
	fd, err := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	s := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path)}
	defer s.dir.Close()
	if err := s.validRoot(); err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(fd, "config", &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	b, err := s.read("config/known_hosts", 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Preview reads a validated nonsecret snapshot without initializing or recovering
// state. A confirmed writer must reopen and compare the returned revision under
// its write lease. Manual profiles need no installation activation record.
func Preview(ctx context.Context, root Root) (Profiles, Revision, error) {
	if err := ctx.Err(); err != nil {
		return Profiles{}, "", err
	}
	fd, err := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Profiles{}, "", ErrOwnership
	}
	s := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path)}
	defer s.dir.Close()
	if err := s.validRoot(); err != nil {
		return Profiles{}, "", err
	}
	read := func(path string, limit int) ([]byte, error) {
		dir, _, _ := strings.Cut(path, "/")
		var st unix.Stat_t
		if err := unix.Fstatat(fd, dir, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return s.read(path, limit)
	}
	var id Identity
	raw, err := read("state/installation.json", 8192)
	if err == nil {
		if err = decodeIdentity(raw, root, &id); err != nil {
			return Profiles{}, "", err
		}
		if id.Purging {
			return Profiles{}, "", ErrPurging
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Profiles{}, "", err
	} else {
		for _, path := range []string{"state/vault.json", "state/vault-usage.json"} {
			if _, e := read(path, 8<<20); !errors.Is(e, os.ErrNotExist) {
				return Profiles{}, "", ErrOwnership
			}
		}
	}
	raw, err = read("config/connections.json", MaxProfileBytes)
	if errors.Is(err, os.ErrNotExist) && !id.ProfilesEstablished {
		raw, err = []byte(`{"version":1,"connections":[]}`), nil
	}
	if err != nil {
		return Profiles{}, "", err
	}
	return DecodeProfiles(bytes.NewReader(raw))
}
