package distribution

import _ "embed"

// LinuxPublicKey is the pinned publisher key, never obtained from release assets.
//
//go:embed linux-public.pem
var LinuxPublicKey []byte
