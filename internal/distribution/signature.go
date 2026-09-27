package distribution

import _ "embed"

// LinuxPublicKey is the pinned publisher key, never obtained from release assets.
//
//go:embed linux-public.pem
var LinuxPublicKey []byte

func candidateSignature(path string) ([]byte, error) {
	f, raw, err := inspect(path + ".sig")
	if err != nil || f.Target != "" || len(raw) != 384 {
		return nil, ErrRelease
	}
	return raw, nil
}
