// Package crypto provides AES-256-GCM encryption at rest for API keys, in
// the enc:v1 format: "enc:v1:<iv hex>:<cipher hex>:<tag hex>", with the AES
// key derived by scrypt from STORAGE_ENCRYPTION_KEY. Without the env var it
// operates in passthrough mode (plaintext stored).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// Prefix marks stored ciphertext.
const Prefix = "enc:v1:"

const (
	ivLen   = 16
	keyLen  = 32
	salt    = "ai-router-field-encryption-v1"
	envName = "STORAGE_ENCRYPTION_KEY"
)

// Cipher encrypts/decrypts key values. It is safe for concurrent use.
type Cipher struct {
	key     []byte
	enabled bool
}

// New builds a Cipher from the STORAGE_ENCRYPTION_KEY env var. If unset or
// blank, encryption is disabled (passthrough).
func New() *Cipher {
	secret := strings.TrimSpace(os.Getenv(envName))
	if secret == "" {
		return &Cipher{}
	}
	key, err := scrypt.Key([]byte(secret), []byte(salt), 32768, 8, 1, keyLen)
	if err != nil {
		slog.Error("crypto: scrypt key derivation failed", "err", err)
		return &Cipher{}
	}
	return &Cipher{key: key, enabled: true}
}

// Enabled reports whether encryption is active.
func (c *Cipher) Enabled() bool { return c != nil && c.enabled }

// LooksEncrypted reports whether v carries the enc:v1: prefix.
func LooksEncrypted(v string) bool { return strings.HasPrefix(v, Prefix) }

// Encrypt returns v unchanged in passthrough mode; otherwise the enc:v1
// ciphertext. Already-encrypted values are returned as-is (no double encrypt).
func (c *Cipher) Encrypt(v string) string {
	if v == "" || !c.Enabled() || LooksEncrypted(v) {
		return v
	}
	iv := make([]byte, ivLen)
	if _, err := rand.Read(iv); err != nil {
		slog.Error("crypto: rand.Read failed", "err", err)
		return ""
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		slog.Error("crypto: aes.NewCipher failed", "err", err)
		return ""
	}
	// enc:v1 pins a 16-byte IV; Go's GCM default is 12, so keep the size.
	aead, err := cipher.NewGCMWithNonceSize(block, ivLen)
	if err != nil {
		slog.Error("crypto: NewGCMWithNonceSize failed", "err", err)
		return ""
	}
	sealed := aead.Seal(nil, iv, []byte(v), nil)
	ct := sealed[:len(sealed)-aead.Overhead()]
	tag := sealed[len(sealed)-aead.Overhead():]
	return Prefix + hex.EncodeToString(iv) + ":" + hex.EncodeToString(ct) + ":" + hex.EncodeToString(tag)
}

// Decrypt returns the plaintext, or "" when the value is not enc:v1 or the
// key no longer matches (auth-tag failure).
func (c *Cipher) Decrypt(v string) string {
	if !LooksEncrypted(v) {
		return v
	}
	if !c.Enabled() {
		slog.Error("crypto: decrypt failed - encryption disabled but value is enc:v1")
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(v, Prefix), ":")
	if len(parts) != 3 {
		slog.Error("crypto: decrypt failed - invalid enc:v1 format", "parts", len(parts))
		return ""
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		slog.Error("crypto: decrypt failed - invalid iv hex", "err", err)
		return ""
	}
	ct, err := hex.DecodeString(parts[1])
	if err != nil {
		slog.Error("crypto: decrypt failed - invalid ciphertext hex", "err", err)
		return ""
	}
	tag, err := hex.DecodeString(parts[2])
	if err != nil {
		slog.Error("crypto: decrypt failed - invalid tag hex", "err", err)
		return ""
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		slog.Error("crypto: decrypt failed - aes.NewCipher", "err", err)
		return ""
	}
	aead, err := cipher.NewGCMWithNonceSize(block, ivLen)
	if err != nil {
		slog.Error("crypto: decrypt failed - NewGCMWithNonceSize", "err", err)
		return ""
	}
	plain, err := aead.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		slog.Error("crypto: decrypt failed - auth tag mismatch", "err", err)
		return ""
	}
	return string(plain)
}
