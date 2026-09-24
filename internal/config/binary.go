package config

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DevelopmentExecutable is the fixed sibling target used only by development
// installation and validation. Runtime consumers use Identity.Executable.
func DevelopmentExecutable(root Root) string {
	return filepath.Join(filepath.Dir(root.Path), "bin", "data-mate")
}

type binaryDirectory struct {
	parent, dir *os.File
	path        string
}

func (d *binaryDirectory) close() { d.dir.Close(); d.parent.Close() }
func (d *binaryDirectory) check() error {
	if checkFD(int(d.parent.Fd()), true) != nil || checkFD(int(d.dir.Fd()), true) != nil || !sameNamed(unix.AT_FDCWD, d.path, d.parent) || !sameNamed(int(d.parent.Fd()), "bin", d.dir) {
		return ErrStale
	}
	return nil
}
func (s *Store) binaryDirectory() (*binaryDirectory, error) {
	if err := s.validRoot(); err != nil {
		return nil, err
	}
	path := filepath.Dir(s.root.Path)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrOwnership
	}
	parent := os.NewFile(uintptr(fd), path)
	if checkFD(fd, true) != nil || !sameNamed(unix.AT_FDCWD, path, parent) {
		parent.Close()
		return nil, ErrOwnership
	}
	bin, err := unix.Openat(fd, "bin", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		parent.Close()
		return nil, err
	}
	d := &binaryDirectory{parent, os.NewFile(uintptr(bin), "bin"), path}
	if err = d.check(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}
func checkBinary(fd int, name string, optional bool) (*os.File, error) {
	f, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if optional && errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, ErrOwnership
	}
	file := os.NewFile(uintptr(f), name)
	var st unix.Stat_t
	if unix.Fstat(f, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0700 || st.Nlink != 1 || !sameNamed(fd, name, file) {
		file.Close()
		return nil, ErrOwnership
	}
	return file, nil
}
