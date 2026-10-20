package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Cipher encrypts sensitive fields with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewFromBase64 builds a cipher from a base64-encoded 32-byte key.
func NewFromBase64(encoded string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode secret key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must decode to 32 bytes, got %d", len(key))
	}
	return New(key)
}

// New builds a cipher from raw key material.
func New(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh nonce and the supplied additional data.
func (c *Cipher) Encrypt(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt opens ciphertext produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext, aad []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce, body := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

// EncryptString is a convenience wrapper.
func (c *Cipher) EncryptString(plaintext string, aad []byte) ([]byte, error) {
	return c.Encrypt([]byte(plaintext), aad)
}

// DecryptString is a convenience wrapper.
func (c *Cipher) DecryptString(ciphertext []byte, aad []byte) (string, error) {
	plain, err := c.Decrypt(ciphertext, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
