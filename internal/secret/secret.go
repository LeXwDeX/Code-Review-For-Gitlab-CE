// Package secret provides authenticated encryption for stored GitLab tokens
// and webhook secrets. Ciphertext format is nonce || ciphertext || GCM tag.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const keySize = 32

// Cipher encrypts and decrypts values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New returns a Cipher for a 32-byte AES-256 key.
func New(key []byte) (*Cipher, error) {
	if len(key) != keySize {
		return nil, errors.New("secret: key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: create GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns nonce || ciphertext || tag. The nonce is random.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	nonceSize := c.aead.NonceSize()
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+c.aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, out); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	return c.aead.Seal(out, out[:nonceSize], plaintext, nil), nil
}

// Decrypt authenticates and decrypts a value produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("secret: ciphertext too short")
	}
	plaintext, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, errors.New("secret: decrypt failed")
	}
	return plaintext, nil
}
