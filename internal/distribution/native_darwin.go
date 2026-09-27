package distribution

import (
	"context"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"golang.org/x/mod/semver"
	"runtime"
	"strings"
)

// VerifyNative authenticates the candidate before execution.
func VerifyNative(ctx context.Context, path string, m Metadata) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || !m.valid() || m.Platform != "darwin_arm64" {
		return ErrRelease
	}
	return verifyCodeSignature(ctx, path, command)
}
func verifyReleaseImage(_ context.Context, path string, m Metadata) error {
	return verifyMachO(path, m.MinimumMacOS)
}

func verifyMachO(path, minimum string) error {
	f, err := macho.Open(path)
	if err != nil {
		return ErrRelease
	}
	defer f.Close()
	if f.Cpu != macho.CpuArm64 || f.Type != macho.TypeExec {
		return ErrRelease
	}
	found := false
	for _, load := range f.Loads {
		if lib, ok := load.(*macho.Dylib); ok && !strings.HasPrefix(lib.Name, "/usr/lib/") && !strings.HasPrefix(lib.Name, "/System/Library/") {
			return ErrRelease
		}
		raw := load.Raw()
		if len(raw) < 8 {
			return ErrRelease
		}
		kind := binary.LittleEndian.Uint32(raw)
		if kind == 0x8000001c {
			return ErrRelease
		} // LC_RPATH
		var version uint32
		if kind == 0x32 { // LC_BUILD_VERSION
			if len(raw) < 24 || binary.LittleEndian.Uint32(raw[8:]) != 1 {
				return ErrRelease
			}
			version = binary.LittleEndian.Uint32(raw[12:])
			found = true
		} else if kind == 0x24 { // LC_VERSION_MIN_MACOSX
			if len(raw) < 16 {
				return ErrRelease
			}
			version = binary.LittleEndian.Uint32(raw[8:])
			found = true
		}
		if version != 0 {
			target := fmt.Sprintf("v%d.%d.%d", version>>16, (version>>8)&255, version&255)
			bound := "v" + minimum
			if strings.Count(minimum, ".") == 1 {
				bound += ".0"
			}
			if semver.Compare(target, bound) > 0 {
				return ErrRelease
			}
		}
	}
	if !found {
		return ErrRelease
	}
	return nil
}
