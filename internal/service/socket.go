package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

type runtimeDir struct {
	file   *os.File
	path   string
	marker unix.Stat_t
	marked bool
	name   string
}

func openRuntime(root config.Root, id identity, create bool) (*runtimeDir, error) {
	path := socketDir(root)
	fresh := false
	if create {
		err := unix.Mkdir(path, 0700)
		if err == nil {
			fresh = true
		} else if !errors.Is(err, unix.EEXIST) {
			return nil, ErrState
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, ErrState
	}
	d := &runtimeDir{file: os.NewFile(uintptr(fd), path), path: path}
	fail := func() (*runtimeDir, error) { d.file.Close(); return nil, ErrState }
	if !d.valid() {
		return fail()
	}
	flags := unix.O_RDONLY
	if fresh {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL
	}
	m, err := unix.Openat(fd, "identity.json", flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return fail()
	}
	marker := os.NewFile(uintptr(m), "identity.json")
	defer marker.Close()
	var st unix.Stat_t
	if unix.Fstat(m, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 {
		return fail()
	}
	if fresh {
		b, _ := json.Marshal(id)
		if _, err = marker.Write(b); err != nil {
			return fail()
		}
		if marker.Sync() != nil || d.file.Sync() != nil {
			return fail()
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(marker, 4097))
		var found identity
		if err != nil || config.DecodeStrict(b, 4096, &found) != nil || found != id {
			return fail()
		}
	}
	d.marker = st
	d.marked = true
	if !d.valid() {
		return fail()
	}
	return d, nil
}
func (d *runtimeDir) valid() bool {
	var st, named unix.Stat_t
	if d.marked {
		var marker unix.Stat_t
		if unix.Fstatat(int(d.file.Fd()), "identity.json", &marker, unix.AT_SYMLINK_NOFOLLOW) != nil || marker.Dev != d.marker.Dev || marker.Ino != d.marker.Ino || marker.Mode&07777 != 0600 || marker.Nlink != 1 {
			return false
		}
	}
	return unix.Fstat(int(d.file.Fd()), &st) == nil && unix.Lstat(d.path, &named) == nil && st.Dev == named.Dev && st.Ino == named.Ino && st.Uid == uint32(os.Geteuid()) && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0700
}
func (d *runtimeDir) socketName() string {
	if d.name != "" {
		return d.name
	}
	return "s"
}
func (d *runtimeDir) socket() string { return filepath.Join(d.path, d.socketName()) }
func (d *runtimeDir) checkSocket() error {
	if !d.valid() {
		return ErrState
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(d.file.Fd()), d.socketName(), &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	if err != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Mode&07777 != 0600 {
		return ErrState
	}
	return nil
}
func (d *runtimeDir) protectSocket() error {
	if !d.valid() {
		return ErrState
	}
	var st unix.Stat_t
	fd := int(d.file.Fd())
	if unix.Fstatat(fd, d.socketName(), &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return ErrState
	}
	if unix.Fchmodat(fd, d.socketName(), 0600, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return ErrState
	}
	return d.checkSocket()
}
func (d *runtimeDir) removeSocket() error {
	err := d.checkSocket()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return unix.Unlinkat(int(d.file.Fd()), d.socketName(), 0)
}

// removeStaleSocket requires a refused connection, not merely a missing launchd
// record. A live orphan/foreign listener must not be unlinked by start or stop.
func (d *runtimeDir) removeStaleSocket() error {
	err := d.checkSocket()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", d.socket(), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return ErrConflict
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return ErrState
	}
	return d.removeSocket()
}
func (d *runtimeDir) cleanup() error {
	d.name = "m"
	if err := d.removeStaleSocket(); err != nil {
		return err
	}
	d.name = "s"
	if err := d.removeStaleSocket(); err != nil {
		return err
	}
	// Never remove unrelated entries or discard the marker that establishes ownership.
	names, err := d.file.Readdirnames(-1)
	if err != nil {
		return err
	}
	if len(names) != 1 || names[0] != "identity.json" {
		return nil
	}
	if !d.valid() {
		return ErrState
	}
	if err = unix.Unlinkat(int(d.file.Fd()), "identity.json", 0); err != nil {
		return err
	}
	return unix.Rmdir(d.path)
}
func peer(conn *net.UnixConn, uid uint32) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, ErrConflict
	}
	var pid int
	var check error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil || cred.Uid != uid {
			check = ErrConflict
			return
		}
		pid, e = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if e != nil || pid <= 0 {
			check = ErrConflict
		}
	})
	if err != nil || check != nil {
		return 0, ErrConflict
	}
	return pid, nil
}

type hello struct {
	Protocol    int      `json:"protocol"`
	Purpose     string   `json:"purpose"`
	Identity    identity `json:"identity"`
	Build       Build    `json:"build"`
	PID         int      `json:"pid"`
	Nonce       string   `json:"nonce"`
	State       string   `json:"state"`
	Error       string   `json:"error"`
	MCPEnabled  bool     `json:"mcp_enabled"`
	KeysetState string   `json:"keyset_state"`
}

func writeHello(w io.Writer, h hello) error {
	b, err := json.Marshal(h)
	if err != nil || len(b) > 4096 {
		return ErrState
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	_, err = io.Copy(w, bytes.NewReader(append(size[:], b...)))
	return err
}
func readHello(r io.Reader) (hello, error) {
	var size [4]byte
	var h hello
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return h, ErrUnavailable
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > 4096 {
		return h, ErrConflict
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return h, ErrUnavailable
	}
	if config.DecodeStrict(b, 4096, &h) != nil {
		return h, ErrConflict
	}
	return h, nil
}
func connect(ctx context.Context, root config.Root, r record, build Build, purpose string) (*net.UnixConn, hello, error) {
	d, err := openRuntime(root, r.Identity, false)
	if err != nil {
		return nil, hello{}, ErrUnavailable
	}
	defer d.file.Close()
	if purpose != "session" {
		d.name = "m"
	}
	if err = d.checkSocket(); err != nil {
		return nil, hello{}, ErrUnavailable
	}
	dialer := net.Dialer{Timeout: time.Second}
	c, err := dialer.DialContext(ctx, "unix", d.socket())
	if err != nil {
		return nil, hello{}, ErrUnavailable
	}
	conn := c.(*net.UnixConn)
	fail := func(e error) (*net.UnixConn, hello, error) { conn.Close(); return nil, hello{}, e }
	pid, err := peer(conn, uint32(os.Geteuid()))
	if err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	h := hello{Protocol: 2, Purpose: purpose, Identity: r.Identity, Build: build, PID: os.Getpid(), Nonce: r.Nonce}
	if err = writeHello(conn, h); err != nil {
		return fail(ErrUnavailable)
	}
	reply, err := readHello(conn)
	if err != nil {
		return fail(err)
	}
	if reply.Identity != r.Identity || reply.Nonce != r.Nonce || reply.PID != pid || (r.PID != 0 && r.PID != pid) || reply.Purpose != purpose {
		return fail(ErrConflict)
	}
	if reply.Protocol != 2 || reply.Build != build || reply.Error == "restart" {
		return fail(ErrRestart)
	}
	if purpose == "management" && !executablePeer(pid, reply.Build.Fingerprint) {
		return fail(ErrConflict)
	}
	if reply.Error != "" {
		return fail(ErrUnavailable)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply, nil
}
