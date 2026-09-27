// Package nativebus connects only to the invoking account's local user bus.
package nativebus

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/sys/unix"
)

// Connect ignores ambient D-Bus addresses, preventing TCP and autolaunch fallbacks.
func Connect(ctx context.Context) (*dbus.Conn, error) {
	path := "/run/user/" + strconv.Itoa(os.Geteuid())
	if config.CheckPath(path) != nil {
		return nil, errors.New("unsafe user bus directory")
	}
	var st unix.Stat_t
	if unix.Lstat(path+"/bus", &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, errors.New("user bus unavailable")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	socket, err := dialer.DialContext(ctx, "unix", path+"/bus")
	if err != nil {
		return nil, err
	}
	raw, err := socket.(*net.UnixConn).SyscallConn()
	var cred *unix.Ucred
	var check error
	if err == nil {
		err = raw.Control(func(fd uintptr) { cred, check = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	}
	if err != nil || check != nil || cred == nil || cred.Uid != uint32(os.Geteuid()) {
		socket.Close()
		return nil, errors.New("user bus identity mismatch")
	}
	conn, err := dbus.NewConn(socket, dbus.WithContext(ctx))
	if err != nil {
		socket.Close()
		return nil, err
	}
	if err = conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err == nil {
		err = conn.Hello()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
