package distribution

/*
#include <pwd.h>
#include <unistd.h>
#include <stdlib.h>
*/
import "C"
import (
	"context"
	"path/filepath"
)

// LoginShell uses the OS account database, independent of SHELL and HOME.
func LoginShell(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var entry C.struct_passwd
	var result *C.struct_passwd
	// Bound NSS output rather than trusting an unbounded passwd record.
	buf := C.malloc(65536)
	if buf == nil {
		return "", ErrConflict
	}
	defer C.free(buf)
	if C.getpwuid_r(C.geteuid(), &entry, (*C.char)(buf), 65536, &result) != 0 || result == nil || entry.pw_shell == nil {
		return "", ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return filepath.Base(C.GoString(entry.pw_shell)), nil
}
