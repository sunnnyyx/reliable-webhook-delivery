// Package security owns credentials, identifiers, and authenticated secret storage.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

func ID() string {
	var b [16]byte
	_, e := rand.Read(b[:])
	if e != nil {
		panic("system randomness unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func ValidID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func Token() (string, []byte, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", nil, e
	}
	token := "rf_" + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token), nil
}
func Hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
func ValidToken(s string) bool {
	if len(s) != 46 || !strings.HasPrefix(s, "rf_") {
		return false
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(s[3:])
	return e == nil && len(b) == 32
}

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("ENCRYPTION_KEY must encode exactly 32 bytes")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	return &Vault{a}, nil
}
func associated(tenant, endpoint, secret string) []byte {
	b, _ := json.Marshal([]string{"relayforge-secret-v1", tenant, endpoint, secret})
	return b
}
func (v *Vault) Encrypt(tenant, endpoint, secret string, plain []byte) ([]byte, []byte, error) {
	if len(plain) != 32 {
		return nil, nil, errors.New("signing secret must have 32 bytes")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, nil, e
	}
	return v.aead.Seal(nil, nonce, plain, associated(tenant, endpoint, secret)), nonce, nil
}
func (v *Vault) Decrypt(tenant, endpoint, secret string, encrypted, nonce []byte) ([]byte, error) {
	if len(nonce) != v.aead.NonceSize() {
		return nil, errors.New("invalid secret nonce")
	}
	return v.aead.Open(nil, nonce, encrypted, associated(tenant, endpoint, secret))
}
