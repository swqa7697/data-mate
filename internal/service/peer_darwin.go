package service

/*
#include <libproc.h>
#include <stdlib.h>
*/
import "C"
import (
	"golang.org/x/sys/unix"
	"net"
	"unsafe"
)

// executablePeer verifies the native PID's executable before management secrets
// cross the socket. The application identity handshake separately detects rebuilds.
func executablePeer(pid int, expected string) bool {
	var path [4096]C.char
	return expected != "" && peerPathHash(pid, &path) == expected
}
func peerPathHash(pid int, path *[4096]C.char) string {
	n := C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path)))
	if n <= 0 {
		return ""
	}
	hash, err := executableHash(C.GoString(&path[0]))
	if err != nil {
		return ""
	}
	return hash
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
