package distribution

import "golang.org/x/sys/unix"

func renameExclusive(a int, from string, b int, to string) error {
	return unix.RenameatxNp(a, from, b, to, unix.RENAME_EXCL)
}
