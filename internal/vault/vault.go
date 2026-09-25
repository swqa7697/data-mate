// Package vault owns Tink credential encryption and the OS keyset boundary.
package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	tinkpb "github.com/tink-crypto/tink-go/v2/proto/tink_go_proto"
	"github.com/tink-crypto/tink-go/v2/tink"
)

const (
	MaxPlaintextBytes = 4 << 20
	MaxSecretBytes    = 128 << 10
	MaxKeysetBytes    = 64 << 10
	MaxReservations   = 1_000_000
)

// Secrets exist only in private memory and authenticated bundle plaintext.
type Secrets struct {
	Password         string `json:"password,omitempty"`
	SSHPassword      string `json:"ssh_password,omitempty"`
	SSHPrivateKey    string `json:"ssh_private_key,omitempty"`
	SSHKeyPassphrase string `json:"ssh_key_passphrase,omitempty"`
	ProxyPassword    string `json:"proxy_password,omitempty"`
}

func (s Secrets) valid() bool {
	for _, v := range []string{s.Password, s.SSHPassword, s.SSHPrivateKey, s.SSHKeyPassphrase, s.ProxyPassword} {
		if len(v) > MaxSecretBytes || !utf8.ValidString(v) {
			return false
		}
	}
	return true
}
func (Secrets) String() string   { return "[redacted credentials]" }
func (Secrets) GoString() string { return "[redacted credentials]" }

type bundle struct {
	Version int     `json:"version"`
	Secrets Secrets `json:"secrets"`
}

func (b *bundle) UnmarshalJSON(raw []byte) error {
	type plain bundle
	var p plain
	if config.DecodeStrict(raw, MaxPlaintextBytes, &p) != nil || p.Version != 1 || !p.Secrets.valid() {
		return ErrRepair
	}
	*b = bundle(p)
	return nil
}

// Patch omits kept fields; an explicit empty value clears a field.
// This private management type must never be returned in a response.
type Patch map[string]string

func (Patch) String() string   { return "[redacted credential patch]" }
func (Patch) GoString() string { return "[redacted credential patch]" }
func (p Patch) Apply(s *Secrets) error {
	for k, v := range p {
		if len(v) > MaxSecretBytes || !utf8.ValidString(v) {
			return ErrRepair
		}
		switch k {
		case "password":
			s.Password = v
		case "ssh_password":
			s.SSHPassword = v
		case "ssh_private_key":
			s.SSHPrivateKey = v
		case "ssh_key_passphrase":
			s.SSHKeyPassphrase = v
		case "proxy_password":
			s.ProxyPassword = v
		default:
			return ErrRepair
		}
	}
	return nil
}
func (p Patch) Complete(profile config.Profile) bool {
	if _, ok := p["password"]; !ok {
		return false
	}
	if profile.Transport.SSH != nil {
		k := "ssh_password"
		if profile.Transport.SSH.Auth == "key" {
			k = "ssh_private_key"
		}
		if _, ok := p[k]; !ok {
			return false
		}
	}
	if profile.Transport.Proxy != nil && profile.Transport.Proxy.Username != "" {
		if _, ok := p["proxy_password"]; !ok {
			return false
		}
	}
	return true
}
func fingerprint(key []byte) string { h := sha256.Sum256(key); return hex.EncodeToString(h[:]) }
func validFingerprint(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func aad(id config.Identity, p config.Profile, version int) []byte {
	b, _ := json.Marshal([]string{"data-mate/credential-bundle", strconv.Itoa(version), id.RootDigest, id.ID, p.ID, p.CredentialRef})
	return b
}
func generateKeyset() ([]byte, error) {
	h, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		return nil, ErrUnavailable
	}
	var b bytes.Buffer
	if insecurecleartextkeyset.Write(h, keyset.NewBinaryWriter(&b)) != nil {
		return nil, ErrUnavailable
	}
	return b.Bytes(), nil
}
func parseKeyset(raw []byte) (tink.AEAD, error) {
	if len(raw) == 0 || len(raw) > MaxKeysetBytes {
		return nil, ErrRepair
	}
	h, err := insecurecleartextkeyset.Read(keyset.NewBinaryReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, ErrRepair
	}
	info := h.KeysetInfo()
	if len(info.KeyInfo) != 1 {
		return nil, ErrRepair
	}
	k := info.KeyInfo[0]
	if k.Status != tinkpb.KeyStatusType_ENABLED || k.KeyId != info.PrimaryKeyId || k.OutputPrefixType != tinkpb.OutputPrefixType_TINK || k.TypeUrl != "type.googleapis.com/google.crypto.tink.AesGcmKey" {
		return nil, ErrRepair
	}
	// Tink validates key size and key material while constructing the primitive.
	material := insecurecleartextkeyset.KeysetMaterial(h)
	if len(material.Key) != 1 || material.Key[0].KeyData.KeyMaterialType != tinkpb.KeyData_SYMMETRIC {
		return nil, ErrRepair
	}
	primitive, err := aead.New(h)
	if err != nil {
		return nil, ErrRepair
	}
	// AES128 is a valid Tink key, but is outside this installation format.
	// The serialized AES-GCM key proto is decoded by the pinned Tink implementation.
	if !aes256(material.Key[0].KeyData.Value) {
		return nil, ErrRepair
	}
	return primitive, nil
}
