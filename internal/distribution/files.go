package distribution

import (
	"context"
	"errors"
	"fmt"
	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"time"
)

// File and Artifact decode receipts written by older releases only.
type File struct {
	Hash   string `json:"hash"`
	Target string `json:"target"`
	Mode   uint32 `json:"mode"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type Artifact struct {
	Path string `json:"path"`
	File File   `json:"file"`
}
type DirectoryError struct {
	Path  string
	Cause error
}

func (e *DirectoryError) Error() string {
	return fmt.Sprintf("cannot use installation directory %q: %v", e.Path, fileCause(e.Cause))
}
func (e *DirectoryError) Unwrap() error { return e.Cause }
func absent(path string) bool           { _, err := os.Lstat(path); return os.IsNotExist(err) }

func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".data-mate.*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func stageCopy(source, dir string, mode os.FileMode) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	f, err := os.CreateTemp(dir, ".data-mate.*")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(f.Name())
		}
	}()
	n, err := io.Copy(f, io.LimitReader(in, MaxBinary+1))
	if err != nil {
		return "", err
	}
	if n > MaxBinary {
		return "", ErrRelease
	}
	if err = f.Chmod(mode); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return f.Name(), nil
}
func replaceLink(target, path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".data-mate.*")
	if err != nil {
		return err
	}
	f.Close()
	os.Remove(f.Name())
	defer os.Remove(f.Name())
	if err = os.Symlink(target, f.Name()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func removeFile(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		return &ArtifactError{path, ErrConflict}
	}
	return os.Remove(path)
}
func removeEmpty(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Keep user-managed directory links, including when their target is empty.
	if st.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	err = os.Remove(path)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) || os.IsNotExist(err) {
		return nil
	}
	return err
}

type distributionLease struct {
	file *os.File
	root *os.File
	path string
	name string
}

func acquire(ctx context.Context, root config.Root) (*distributionLease, error) {
	return acquireObserved(ctx, root, nil)
}
func acquireObserved(ctx context.Context, root config.Root, waiting func()) (*distributionLease, error) {
	return acquireNamed(ctx, root, "distribution.lock", waiting)
}
func acquireNamed(ctx context.Context, root config.Root, name string, waiting func()) (*distributionLease, error) {
	if root.Environment.Kind() != config.Production {
		return nil, ErrConflict
	}
	dir, err := os.Open(root.Path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		dir.Close()
		return nil, &os.PathError{Op: "open distribution lock", Path: filepath.Join(root.Path, name), Err: err}
	}
	l := &distributionLease{os.NewFile(uintptr(fd), name), dir, root.Path, name}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Mode&unix.S_IFMT != unix.S_IFREG {
		l.close()
		return nil, ErrConflict
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			l.close()
			return nil, err
		}
		if waiting != nil {
			waiting()
			waiting = nil
		}
		select {
		case <-ctx.Done():
			l.close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = l.check(); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}
func (l *distributionLease) check() error {
	for _, pair := range []struct {
		f    *os.File
		path string
	}{{l.root, l.path}, {l.file, filepath.Join(l.path, l.name)}} {
		opened, err := pair.f.Stat()
		named, e := os.Stat(pair.path)
		if err != nil || e != nil || !os.SameFile(opened, named) {
			return config.ErrStale
		}
	}
	return nil
}
func (l *distributionLease) close() {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	l.file.Close()
	l.root.Close()
}

func fileCause(err error) string {
	var path *os.PathError
	if errors.As(err, &path) {
		return path.Op + ": " + path.Err.Error()
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	if errors.Is(err, ErrConflict) {
		return "path is occupied by an unrelated file or directory"
	}
	return "filesystem operation failed"
}
