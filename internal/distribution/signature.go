package distribution

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"io"
	"os"
)

// LinuxPublicKey is the pinned publisher key, never obtained from release assets.
//
//go:embed linux-public.pem
var LinuxPublicKey []byte

func verifyLinuxSignature(path string, public []byte) error {
	block, rest := pem.Decode(public)
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		return ErrRelease
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return ErrRelease
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok || key.N.BitLen() != 3072 {
		return ErrRelease
	}
	sigFile, err := os.Open(path + ".sig")
	if err != nil {
		return ErrRelease
	}
	sig, readErr := io.ReadAll(io.LimitReader(sigFile, 4097))
	closeErr := sigFile.Close()
	if readErr != nil || closeErr != nil || len(sig) != key.Size() {
		return ErrRelease
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrRelease
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxBinary+1))
	if err != nil || n == 0 || n > MaxBinary {
		return ErrRelease
	}
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, h.Sum(nil), sig) != nil {
		return ErrRelease
	}
	return nil
}

func candidateSignature(path string) ([]byte, error) {
	f, raw, err := inspect(path + ".sig")
	if err != nil || f.Target != "" || len(raw) != 384 {
		return nil, ErrRelease
	}
	return raw, nil
}
