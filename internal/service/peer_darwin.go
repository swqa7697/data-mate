package service

/*
#include <libproc.h>
#include <stdlib.h>
*/
import "C"
import "unsafe"

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
	hash, err := executableHash(C.GoString(&path[0]), false)
	if err != nil {
		return ""
	}
	return hash
}
