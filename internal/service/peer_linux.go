package service

import (
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
)

func peer(conn *net.UnixConn, uid uint32) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, ErrConflict
	}
	var cred *unix.Ucred
	var check error
	err = raw.Control(func(fd uintptr) { cred, check = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil || check != nil || cred == nil || cred.Uid != uid || cred.Pid <= 0 {
		return 0, ErrConflict
	}
	return int(cred.Pid), nil
}

// Open the kernel's executable handle, rather than resolving a pathname that
// could now name a replacement executable after an atomic upgrade.
func executablePeer(pid int, expected string) bool {
	if pid <= 0 || expected == "" {
		return false
	}
	f, err := os.Open("/proc/" + itoa(pid) + "/exe")
	if err != nil {
		return false
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Size > 256<<20 {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, (256<<20)+1)); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == expected
}
