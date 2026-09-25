package config

import (
	"bytes"
	"context"
	"errors"
	"os"

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
	b, err := s.read("known_hosts", 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Preview reads a validated nonsecret snapshot without initializing or recovering
// state. A confirmed writer must reopen and compare the returned revision under
// its write lease. An empty uninitialized root remains a passive empty snapshot.
func Preview(ctx context.Context, root Root) (Profiles, Revision, error) {
	if root.Environment.Kind() == Production {
		if _, err := os.Lstat(root.Path); errors.Is(err, os.ErrNotExist) && CheckPath(root.Path) == nil {
			p, digest, err := DecodeProfiles(bytes.NewReader([]byte(`{"version":1,"connections":[]}`)))
			return p, databaseRevision(digest, 0), err
		}
	}
	s, err := OpenExisting(ctx, root)
	if errors.Is(err, os.ErrNotExist) {
		fd, e := unix.Open(root.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return Profiles{}, "", ErrOwnership
		}
		temporary := &Store{root: root, dir: os.NewFile(uintptr(fd), root.Path)}
		defer temporary.dir.Close()
		if e = temporary.validRoot(); e != nil {
			return Profiles{}, "", e
		}
		if e = temporary.checkObsolete(); e != nil {
			return Profiles{}, "", e
		}
		if _, e = os.Lstat(root.Path + "/data-mate.db"); !errors.Is(e, os.ErrNotExist) {
			return Profiles{}, "", ErrOwnership
		}
		p, digest, e := DecodeProfiles(bytes.NewReader([]byte(`{"version":1,"connections":[]}`)))
		return p, databaseRevision(digest, 0), e
	}
	if err != nil {
		return Profiles{}, "", err
	}
	defer s.Close()
	l, err := s.ReadLease(ctx)
	if err != nil {
		return Profiles{}, "", err
	}
	defer l.Release()
	return l.ProfileSnapshot()
}
