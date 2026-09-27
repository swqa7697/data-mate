package distribution

import "golang.org/x/sys/unix"

func renameExclusive(a int, from string, b int, to string) error {
	return unix.Renameat2(a, from, b, to, unix.RENAME_NOREPLACE)
}
