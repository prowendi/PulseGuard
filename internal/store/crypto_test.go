package store

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func makeKeyB64(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func TestCipher_Roundtrip(t *testing.T) {
	c, err := NewCipher(makeKeyB64(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("hunter2"),
		[]byte("123456789:AAExampleTelegramBotTokenXYZabcdEf"),
		bytes.Repeat([]byte("A"), 4096),
	}
	for i, plain := range cases {
		blob, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("case %d encrypt: %v", i, err)
		}
		if bytes.Contains(blob, plain) && len(plain) > 0 {
			t.Fatalf("case %d: ciphertext leaks plaintext", i)
		}
		got, err := c.Decrypt(blob)
		if err != nil {
			t.Fatalf("case %d decrypt: %v", i, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("case %d: got %x want %x", i, got, plain)
		}
	}
}

func TestCipher_NonceRandomness(t *testing.T) {
	c, err := NewCipher(makeKeyB64(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	a, err := c.Encrypt([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("same plaintext produced identical ciphertext; nonce reuse")
	}
}

func TestCipher_RejectsShortKey(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("only-16-bytes-key"))
	if _, err := NewCipher(short); err == nil {
		t.Fatalf("expected error for non-32-byte key")
	}
}

func TestCipher_RejectsEmptyKey(t *testing.T) {
	if _, err := NewCipher(""); err == nil {
		t.Fatalf("expected error for empty key")
	}
}

func TestCipher_RejectsBadBase64(t *testing.T) {
	if _, err := NewCipher("***not base64***"); err == nil {
		t.Fatalf("expected error for non-base64 input")
	}
}

func TestCipher_DetectsTamper(t *testing.T) {
	c, err := NewCipher(makeKeyB64(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// flip a bit in the ciphertext body
	idx := c.aead.NonceSize() + 1
	if idx >= len(blob) {
		t.Skip("blob too small to tamper")
	}
	blob[idx] ^= 0x01
	if _, err := c.Decrypt(blob); err == nil {
		t.Fatalf("tampered ciphertext should fail Open")
	}
}

func TestCipher_RejectsTooShort(t *testing.T) {
	c, err := NewCipher(makeKeyB64(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := c.Decrypt([]byte("short")); err == nil {
		t.Fatalf("expected error for truncated blob")
	}
}

// TestCipher_RejectsExampleWeakKey is the regression guard for
// security-report S-M5: the documented example value (32 ASCII 'a')
// must NEVER be accepted at boot. NewCipher returns the ErrWeakMasterKey
// sentinel so runtime can surface a precise operator message.
func TestCipher_RejectsExampleWeakKey(t *testing.T) {
	weak := bytes.Repeat([]byte{0x61}, 32) // matches config.example.yaml
	enc := base64.StdEncoding.EncodeToString(weak)
	_, err := NewCipher(enc)
	if err == nil {
		t.Fatal("expected error for example weak master_key")
	}
	if !errors.Is(err, ErrWeakMasterKey) {
		t.Fatalf("expected ErrWeakMasterKey, got %v", err)
	}
}
