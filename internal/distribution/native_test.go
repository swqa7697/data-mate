package distribution

import (
	"debug/macho"
	"os"
	"path/filepath"
	"testing"

	"github.com/swqa7697/data-mate/internal/config"
)

// New native boundary: existing Keychain/lifecycle opt-ins cannot establish trust
// for a signed bare Mach-O candidate before its first execution.
func TestNativeRelease(t *testing.T) {
	path := os.Getenv("DATA_MATE_DISTRIBUTION_CANDIDATE")
	if os.Getenv("DATA_MATE_NATIVE_TEST") != "1" || !filepath.IsAbs(path) {
		t.Skip("requires DATA_MATE_NATIVE_TEST=1 and an absolute DATA_MATE_DISTRIBUTION_CANDIDATE Developer ID-signed artifact")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "release.txt"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ParseMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyNative(t.Context(), path, meta); err != nil {
		t.Fatal("native publisher/compatibility check", err)
	}
	if _, err = command(t.Context(), "/bin/bash", "-c", `source "$1"; verify_signature "$2"`, "verification", "../../scripts/install-release.sh", path); err != nil {
		t.Fatal("bootstrap publisher check", err)
	}
	output, err := command(t.Context(), path, "__release-metadata")
	var actual Metadata
	if err != nil || config.DecodeStrict(output, 4096, &actual) != nil || actual != meta {
		t.Fatal("verified candidate contract", err)
	}
	binary, err := os.ReadFile(path)
	if err != nil || len(binary) < 4096 {
		t.Fatal("candidate read", err)
	}
	image, err := macho.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	text := image.Section("__text")
	image.Close()
	if text == nil || int(text.Offset) >= len(binary) {
		t.Fatal("missing executable text section")
	}
	binary[text.Offset] ^= 1
	altered := filepath.Join(t.TempDir(), "altered")
	if err = os.WriteFile(altered, binary, 0700); err != nil {
		t.Fatal(err)
	}
	if err = VerifyNative(t.Context(), altered, meta); err == nil {
		t.Fatal("altered signature accepted")
	}
	if _, err = command(t.Context(), "/bin/bash", "-c", `source "$1"; verify_signature "$2"`, "verification", "../../scripts/install-release.sh", altered); err == nil {
		t.Fatal("bootstrap accepted altered signature")
	}
}
