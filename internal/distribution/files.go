package distribution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

// File records exact identity as well as bytes or a symlink target.
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

func inspect(path string) (File, []byte, error) {
	if config.CheckPath(filepath.Dir(path)) != nil {
		return File{}, nil, ErrConflict
	}
	st, err := os.Lstat(path)
	if err != nil {
		return File{}, nil, err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || (!st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0) || stat.Nlink != 1 {
		return File{}, nil, ErrConflict
	}
	f := File{Mode: uint32(st.Mode().Perm()), Device: uint64(stat.Dev), Inode: stat.Ino}
	if st.Mode()&os.ModeSymlink != 0 {
		f.Target, err = os.Readlink(path)
		return f, nil, err
	}
	if st.Mode().Perm()&0022 != 0 || st.Size() > MaxBinary {
		return File{}, nil, ErrConflict
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return File{}, nil, ErrConflict
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return File{}, nil, ErrConflict
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxBinary+1))
	if err != nil || len(raw) > MaxBinary {
		return File{}, nil, ErrConflict
	}
	now, err := os.Lstat(path)
	if err != nil || !os.SameFile(st, now) {
		return File{}, nil, ErrConflict
	}
	f.Hash = digest(raw)
	return f, raw, nil
}
func contentEqual(a, b File) bool {
	return a.Hash == b.Hash && a.Target == b.Target && (a.Target != "" || a.Mode == b.Mode)
}
func exact(path string, expected File) error {
	f, _, err := inspect(path)
	if err != nil || !contentEqual(f, expected) || (expected.Inode != 0 && (f.Inode != expected.Inode || f.Device != expected.Device)) {
		return ErrConflict
	}
	return nil
}
func absent(path string) bool { _, err := os.Lstat(path); return errors.Is(err, os.ErrNotExist) }
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// pinnedDirectory walks with O_NOFOLLOW and retains every ancestor descriptor.
// Mutations cannot be redirected through a swapped symlink or directory chain.
type pinnedDirectory struct {
	files []*os.File
	names []string
}

func pinParent(path string) (*pinnedDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConflict
	}
	root, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	p := &pinnedDirectory{files: []*os.File{root}}
	for _, name := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		if name == "" {
			continue
		}
		fd, err := unix.Openat(int(p.last().Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			p.close()
			return nil, err
		}
		f := os.NewFile(uintptr(fd), name)
		p.files = append(p.files, f)
		p.names = append(p.names, name)
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || (st.Uid != uint32(os.Geteuid()) && st.Uid != 0) || (st.Mode&0022 != 0 && !(st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)) {
			p.close()
			return nil, ErrConflict
		}
	}
	if err = p.check(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}
func (p *pinnedDirectory) last() *os.File { return p.files[len(p.files)-1] }
func (p *pinnedDirectory) close() {
	for i := len(p.files) - 1; i >= 0; i-- {
		p.files[i].Close()
	}
}
func (p *pinnedDirectory) check() error {
	for i, name := range p.names {
		var opened, named unix.Stat_t
		if unix.Fstat(int(p.files[i+1].Fd()), &opened) != nil || unix.Fstatat(int(p.files[i].Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || opened.Dev != named.Dev || opened.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFDIR {
			return config.ErrStale
		}
	}
	return nil
}
func writeNew(path string, raw []byte, mode os.FileMode) error {
	p, err := pinParent(path)
	if err != nil {
		return err
	}
	defer p.close()
	fd, err := unix.Openat(int(p.last().Fd()), filepath.Base(path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode))
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(mode)
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
	if err = p.check(); err != nil {
		return err
	}
	return p.last().Sync()
}
func removeExact(path string, expected File) error {
	if absent(path) {
		return nil
	}
	p, err := pinParent(path)
	if err != nil {
		return err
	}
	defer p.close()
	if exact(path, expected) != nil || p.check() != nil {
		return ErrConflict
	}
	if err := unix.Unlinkat(int(p.last().Fd()), filepath.Base(path), 0); err != nil {
		return err
	}
	return p.last().Sync()
}
func renameExact(from, to string, expected File) error {
	src, err := pinParent(from)
	if err != nil {
		return err
	}
	defer src.close()
	dst, err := pinParent(to)
	if err != nil {
		return err
	}
	defer dst.close()
	if exact(from, expected) != nil || src.check() != nil || dst.check() != nil {
		return ErrConflict
	}
	if err = unix.RenameatxNp(int(src.last().Fd()), filepath.Base(from), int(dst.last().Fd()), filepath.Base(to), unix.RENAME_EXCL); err != nil {
		return err
	}
	return dst.last().Sync()
}
func replaceExact(from, to string, before *File) error {
	src, err := pinParent(from)
	if err != nil {
		return err
	}
	defer src.close()
	dst, err := pinParent(to)
	if err != nil {
		return err
	}
	defer dst.close()
	if src.check() != nil || dst.check() != nil {
		return ErrConflict
	}
	if before == nil {
		return unix.RenameatxNp(int(src.last().Fd()), filepath.Base(from), int(dst.last().Fd()), filepath.Base(to), unix.RENAME_EXCL)
	}
	if exact(to, *before) != nil {
		return ErrConflict
	}
	if err = unix.Renameat(int(src.last().Fd()), filepath.Base(from), int(dst.last().Fd()), filepath.Base(to)); err != nil {
		return err
	}
	return dst.last().Sync()
}
func symlinkNew(target, path string) error {
	p, err := pinParent(path)
	if err != nil {
		return err
	}
	defer p.close()
	if err = unix.Symlinkat(target, int(p.last().Fd()), filepath.Base(path)); err != nil {
		return err
	}
	return p.last().Sync()
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
	if root.Environment.Kind() != config.Production || config.CheckPath(root.Path) != nil {
		return nil, ErrConflict
	}
	dir, err := os.Open(root.Path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		dir.Close()
		return nil, ErrConflict
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
		named, e := os.Lstat(pair.path)
		if err != nil || e != nil || !os.SameFile(opened, named) {
			return config.ErrStale
		}
	}
	return config.CheckPath(l.path)
}
func (l *distributionLease) close() {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	l.file.Close()
	l.root.Close()
}
