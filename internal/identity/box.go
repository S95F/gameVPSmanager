package identity

// box.go implements AES-256-GCM encryption for class-B secrets (TOTP seeds).
//
// One master key fans out via HKDF-SHA256 into independent per-purpose subkeys.
// Each encryption uses a random 96-bit nonce prepended to the ciphertext.
// No pgcrypto: the key never travels through Postgres.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Box holds a master key and derives per-purpose subkeys.
type Box struct {
	master []byte // 32 bytes (AES-256)
}

// NewBox creates a Box from a 32-byte master key.
func NewBox(master []byte) (*Box, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("identity: box master key must be 32 bytes, got %d", len(master))
	}
	cp := make([]byte, 32)
	copy(cp, master)
	return &Box{master: cp}, nil
}

// aead derives a purpose-bound AEAD from the master key.
func (b *Box) aead(purpose string) (cipher.AEAD, error) {
	reader := hkdf.New(sha256.New, b.master, nil, []byte("gvm/v1/"+purpose))
	dk := make([]byte, 32)
	if _, err := io.ReadFull(reader, dk); err != nil {
		return nil, fmt.Errorf("identity: hkdf: %w", err)
	}
	blk, err := aes.NewCipher(dk)
	if err != nil {
		return nil, fmt.Errorf("identity: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, fmt.Errorf("identity: gcm: %w", err)
	}
	return gcm, nil
}

// Seal encrypts plaintext under purpose, prepending a random nonce.
// The additional data (aad) is authenticated but not encrypted.
func (b *Box) Seal(purpose string, plaintext, aad []byte) ([]byte, error) {
	gcm, err := b.aead(purpose)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("identity: box seal: rand nonce: %w", err)
	}
	ct := gcm.Seal(nonce, nonce, plaintext, aad)
	return ct, nil
}

// Open decrypts ciphertext (nonce || ct) under purpose.
func (b *Box) Open(purpose string, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := b.aead(purpose)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, fmt.Errorf("identity: box open: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("identity: box open: %w", err)
	}
	return plain, nil
}

// SealTOTP encrypts a base32 TOTP secret for storage in users.totp_secret_ct.
// AAD binds it to the user ID so ciphertexts cannot be swapped between users.
func (b *Box) SealTOTP(userID string, secret string) ([]byte, error) {
	return b.Seal("totp", []byte(secret), []byte(userID))
}

// OpenTOTP decrypts a TOTP ciphertext.
func (b *Box) OpenTOTP(userID string, ct []byte) (string, error) {
	plain, err := b.Open("totp", ct, []byte(userID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
