package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrKeyNotConfigured = errors.New("key is not configured")

func Encrypt(key, context, plaintext []byte) ([]byte, error) {
	if len(context) == 0 {
		return nil, fmt.Errorf("encryption context is required")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, context), nil
}

func Decrypt(key, context, ciphertext []byte) ([]byte, error) {
	if len(context) == 0 {
		return nil, fmt.Errorf("encryption context is required")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("ciphertext is too short")
	}
	nonce := ciphertext[:aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, ciphertext[aead.NonceSize():], context)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func Hash(key, value []byte) string {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write(value)
	return hex.EncodeToString(hash.Sum(nil))
}

func EncryptWithKeyID(keys map[string][]byte, keyID string, context, plaintext []byte) ([]byte, error) {
	key, err := keyByID(keys, keyID)
	if err != nil {
		return nil, err
	}
	return Encrypt(key, context, plaintext)
}

func DecryptWithKeyID(keys map[string][]byte, keyID string, context, ciphertext []byte) ([]byte, error) {
	key, err := keyByID(keys, keyID)
	if err != nil {
		return nil, err
	}
	return Decrypt(key, context, ciphertext)
}

func HashWithKeyID(keys map[string][]byte, keyID string, value []byte) (string, error) {
	key, err := keyByID(keys, keyID)
	if err != nil {
		return "", err
	}
	return Hash(key, value), nil
}

func keyByID(keys map[string][]byte, keyID string) ([]byte, error) {
	if keyID == "" {
		return nil, fmt.Errorf("key ID is required")
	}
	key, ok := keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrKeyNotConfigured, keyID)
	}
	return key, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
