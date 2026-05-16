package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Cipher wraps AES-256-GCM with a base64-encoded 32-byte master key.
// The on-disk layout is [12B nonce | ciphertext | 16B tag] (the tag is
// appended automatically by Seal).
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher decodes masterKeyB64 (must be base64 of exactly 32 bytes)
// and returns a ready-to-use AES-GCM cipher.
func NewCipher(masterKeyB64 string) (*Cipher, error) {
	if masterKeyB64 == "" {
		return nil, errors.New("master_key_b64 is empty")
	}
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode master_key_b64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master_key_b64 must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plain under a fresh random nonce, returning nonce||ciphertext||tag.
func (c *Cipher) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	// Pass nonce as dst so Seal appends ciphertext to it: result = nonce||ct||tag.
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt reverses Encrypt. Tampered or truncated blobs return an error.
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns+c.aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("aead open: %w", err)
	}
	return plain, nil
}
