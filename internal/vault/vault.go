package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/config"
)

const (
	MaxEnvelopeBytes  = 8 << 20
	MaxPlaintextBytes = 4 << 20
	MaxSecretBytes    = 128 << 10
	MaxReservations   = 1_000_000
)

// Secrets are private in-memory inputs. Callers must never log this value or
// marshal it outside the encrypted document. Empty values represent no secret.
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

// String prevents accidental formatted diagnostics from containing credentials.
func (Secrets) String() string   { return "[redacted credentials]" }
func (Secrets) GoString() string { return "[redacted credentials]" }

type bundle struct {
	ConnectionID string  `json:"connection_id"`
	Secrets      Secrets `json:"secrets"`
}

// UnmarshalJSON requires both binding and secret object even when all secrets
// are empty; unknown/null fields are rejected before the bundle is usable.
func (b *bundle) UnmarshalJSON(raw []byte) error {
	type plain bundle
	var value plain
	if err := config.DecodeStrict(raw, MaxPlaintextBytes, &value); err != nil {
		return err
	}
	*b = bundle(value)
	return nil
}

type document struct {
	Version int               `json:"version"`
	Bundles map[string]bundle `json:"bundles"`
}
type envelope struct {
	Version       int    `json:"version"`
	VaultID       string `json:"vault_id"`
	Cipher        string `json:"cipher"`
	WriteSequence uint64 `json:"write_sequence"`
	Nonce         []byte `json:"nonce"`
	Ciphertext    []byte `json:"ciphertext"`
}
type usage struct {
	Version        int    `json:"version"`
	InstallationID string `json:"installation_id"`
	RootDigest     string `json:"root_digest"`
	KeyFingerprint string `json:"key_fingerprint"`
	Reserved       uint64 `json:"reserved"`
}
type openedVault struct {
	key      []byte
	ledger   usage
	envelope envelope
	document document
}

func fingerprint(key []byte) string { h := sha256.Sum256(key); return hex.EncodeToString(h[:]) }
func validFingerprint(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func readOptional(l *config.Lease, path string, max int) ([]byte, error) {
	b, e := l.Read(path, max)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	return b, e
}

func (r *Repository) load(ctx context.Context, l *config.Lease, create bool) (_ *openedVault, err error) {
	id := l.Identity()
	raw, err := readOptional(l, "state/vault.json", MaxEnvelopeBytes)
	if err != nil {
		return nil, err
	}
	accounting, err := readOptional(l, "state/vault-usage.json", 8192)
	if err != nil {
		return nil, err
	}
	v := &openedVault{document: document{Version: 1, Bundles: map[string]bundle{}}}
	defer func() {
		if err != nil {
			clear(v.key)
		}
	}()

	if raw != nil {
		if err := decodeEnvelope(raw, &v.envelope); err != nil {
			return nil, err
		}
	}

	if accounting != nil {
		if config.DecodeStrict(accounting, 8192, &v.ledger) != nil || v.ledger.Version != 1 || v.ledger.InstallationID != id.ID || v.ledger.RootDigest != id.RootDigest || !validFingerprint(v.ledger.KeyFingerprint) || v.ledger.Reserved > MaxReservations {
			return nil, ErrRepair
		}
	}
	// Empty installations do not retrieve/create a key on nonsecret operations.
	if raw == nil && accounting == nil && !create {
		return v, nil
	}
	v.key, err = r.keys.Load(ctx, id.RootDigest)
	if err != nil && !errors.Is(err, ErrMissing) {
		return nil, providerError(err)
	}
	if errors.Is(err, ErrMissing) {
		if raw != nil {
			return nil, ErrRepair
		}
		if !create {
			return nil, ErrRepair
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		key := make([]byte, 32)
		defer clear(key)
		if _, err = rand.Read(key); err != nil {
			return nil, ErrUnavailable
		}
		v.ledger = usage{Version: 1, InstallationID: id.ID, RootDigest: id.RootDigest, KeyFingerprint: fingerprint(key)}
		b, _ := json.Marshal(v.ledger)
		if err = l.Replace("state/vault-usage.json", b); err != nil {
			return nil, err
		}
		if err = r.point("before-key-create"); err != nil {
			return nil, err
		}
		v.key, err = r.keys.CreateIfAbsent(ctx, id.RootDigest, key)
		if err != nil {
			return nil, providerError(err)
		}
		if err = r.point("after-key-create"); err != nil {
			return nil, err
		}
	} else if accounting == nil {
		return nil, ErrRepair
	}
	if len(v.key) != 32 || fingerprint(v.key) != v.ledger.KeyFingerprint {
		return nil, ErrRepair
	}
	if raw == nil {
		return v, nil
	}
	if v.envelope.WriteSequence > v.ledger.Reserved {
		return nil, ErrRepair
	}
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, ErrRepair
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrRepair
	}
	plain, err := gcm.Open(nil, v.envelope.Nonce, v.envelope.Ciphertext, aad(v.envelope, id))
	if err != nil {
		return nil, ErrRepair
	}
	defer clear(plain)
	if config.DecodeStrict(plain, MaxPlaintextBytes, &v.document) != nil || !validDocument(v.document) {
		return nil, ErrRepair
	}
	return v, nil
}
func decodeEnvelope(raw []byte, e *envelope) error {
	// Validate encoded lengths before allocating decoded nonce/ciphertext buffers.
	var wire struct {
		Version       int    `json:"version"`
		VaultID       string `json:"vault_id"`
		Cipher        string `json:"cipher"`
		WriteSequence uint64 `json:"write_sequence"`
		Nonce         string `json:"nonce"`
		Ciphertext    string `json:"ciphertext"`
	}
	if config.DecodeStrict(raw, MaxEnvelopeBytes, &wire) != nil || wire.Version != 1 || wire.Cipher != "AES-256-GCM" || !config.ValidUUID(wire.VaultID) || wire.WriteSequence == 0 || len(wire.Nonce) != 16 || len(wire.Ciphertext) > base64.StdEncoding.EncodedLen(MaxPlaintextBytes+16) {
		return ErrRepair
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(wire.Nonce)
	if err != nil || len(nonce) != 12 {
		return ErrRepair
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(wire.Ciphertext)
	if err != nil || len(ciphertext) < 16 || len(ciphertext) > MaxPlaintextBytes+16 {
		return ErrRepair
	}
	*e = envelope{Version: wire.Version, VaultID: wire.VaultID, Cipher: wire.Cipher, WriteSequence: wire.WriteSequence, Nonce: nonce, Ciphertext: ciphertext}
	return nil
}

func validDocument(d document) bool {
	if d.Version != 1 || d.Bundles == nil || len(d.Bundles) > 256 {
		return false
	}
	for ref, b := range d.Bundles {
		if !config.ValidUUID(ref) || !config.ValidUUID(b.ConnectionID) || !b.Secrets.valid() {
			return false
		}
	}
	return true
}
func aad(e envelope, id config.Identity) []byte {
	var b []byte
	for _, value := range []string{"1", e.Cipher, e.VaultID, id.RootDigest, id.ID} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(value)))
		b = append(b, value...)
	}
	b = binary.BigEndian.AppendUint32(b, 8)
	return binary.BigEndian.AppendUint64(b, e.WriteSequence)
}
func (r *Repository) publish(ctx context.Context, l *config.Lease, v *openedVault) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validDocument(v.document) {
		return ErrRepair
	}
	plain, err := json.Marshal(v.document)
	if err != nil {
		return ErrRepair
	}
	defer clear(plain)
	if len(plain) > MaxPlaintextBytes {
		return errors.New("credential vault exceeds size limit")
	}
	if v.ledger.Reserved >= MaxReservations {
		return ErrLimit
	}
	if v.envelope.VaultID == "" {
		v.envelope.VaultID, err = config.NewID()
		if err != nil {
			return err
		}
	}
	v.envelope.Version = 1
	v.envelope.Cipher = "AES-256-GCM"
	v.ledger.Reserved++
	accounting, _ := json.Marshal(v.ledger)
	if err = l.Replace("state/vault-usage.json", accounting); err != nil {
		return err
	}
	// No Seal is possible before the reservation has reached durable storage.
	if err = r.point("after-reservation"); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	v.envelope.WriteSequence = v.ledger.Reserved
	v.envelope.Nonce = make([]byte, 12)
	if _, err = rand.Read(v.envelope.Nonce); err != nil {
		return ErrUnavailable
	}
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return ErrRepair
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ErrRepair
	}
	v.envelope.Ciphertext = gcm.Seal(nil, v.envelope.Nonce, plain, aad(v.envelope, l.Identity()))
	if err = r.point("after-encryption"); err != nil {
		return err
	}
	raw, err := json.Marshal(v.envelope)
	if err != nil || len(raw) > MaxEnvelopeBytes {
		return ErrRepair
	}
	if err = l.Replace("state/vault.json", raw); err != nil {
		return err
	}
	return nil
}
