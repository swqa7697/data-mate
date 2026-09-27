package distribution

import (
	"context"
	"debug/elf"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

// VerifyNative authenticates publisher and platform before executing any code.
func VerifyNative(ctx context.Context, path string, m Metadata) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runtime.GOARCH != "amd64" || os.Geteuid() == 0 || !m.valid() || m.Platform != "linux_amd64" {
		return ErrRelease
	}
	if err := verifyLinuxSignature(path, LinuxPublicKey); err != nil {
		return err
	}
	f, err := elf.Open(path)
	if err != nil {
		return ErrRelease
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_X86_64 || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return ErrRelease
	}
	interpreters := 0
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		interpreters++
		raw, err := io.ReadAll(io.LimitReader(p.Open(), 4097))
		if err != nil || (string(raw) != "/lib64/ld-linux-x86-64.so.2\x00" && string(raw) != "/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2\x00") {
			return ErrRelease
		}
	}
	if interpreters != 1 {
		return ErrRelease
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return ErrRelease
	}
	for _, lib := range libs {
		switch lib {
		case "libc.so.6", "libpthread.so.0", "libdl.so.2", "libm.so.6", "librt.so.1", "ld-linux-x86-64.so.2":
		default:
			return ErrRelease
		}
	}
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		paths, e := f.DynString(tag)
		if e != nil || len(paths) != 0 {
			return ErrRelease
		}
	}
	raw, err := command(ctx, "/usr/bin/getconf", "GNU_LIBC_VERSION")
	if err != nil {
		return err
	}
	host := strings.Fields(string(raw))
	if len(host) != 2 || host[0] != "glibc" || !osVersion.MatchString(host[1]) {
		return ErrRelease
	}
	normalize := func(v string) string {
		if strings.Count(v, ".") == 1 {
			v += ".0"
		}
		return "v" + v
	}
	if !semver.IsValid(normalize(m.MinimumGlibc)) || semver.Compare(normalize(host[1]), normalize(m.MinimumGlibc)) < 0 {
		return ErrRelease
	}
	return ctx.Err()
}
