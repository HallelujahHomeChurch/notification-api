package crypto

import (
	"bytes"
	"regexp"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	context := []byte("message-id:payload")
	plaintext := []byte(`{"verifyUrl":"https://example.test/token"}`)

	ciphertext, err := Encrypt(key, context, plaintext)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("Encrypt() returned plaintext")
	}

	decoded, err := Decrypt(key, context, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(decoded, plaintext) {
		t.Fatalf("Decrypt() = %q, want %q", decoded, plaintext)
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	context := []byte("message-id:target")
	plaintext := []byte("same input")

	first, err := Encrypt(key, context, plaintext)
	if err != nil {
		t.Fatalf("first Encrypt() error = %v", err)
	}
	second, err := Encrypt(key, context, plaintext)
	if err != nil {
		t.Fatalf("second Encrypt() error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("Encrypt() reused a nonce")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	context := []byte("message-id:payload")
	ciphertext, err := Encrypt(key, context, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	ciphertext[len(ciphertext)-1] ^= 1

	if _, err := Decrypt(key, context, ciphertext); err == nil {
		t.Fatal("Decrypt() error = nil, want tampered ciphertext rejected")
	}
}

func TestDecryptRejectsDifferentContext(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	ciphertext, err := Encrypt(key, []byte("message-id:target"), []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	if _, err := Decrypt(key, []byte("message-id:payload"), ciphertext); err == nil {
		t.Fatal("Decrypt() error = nil, want different context rejected")
	}
}

func TestEncryptRejectsEmptyContext(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	if _, err := Encrypt(key, nil, []byte("secret")); err == nil {
		t.Fatal("Encrypt() error = nil, want empty context rejected")
	}
}

func TestHashReturnsLowercaseHMACSHA256(t *testing.T) {
	got := Hash([]byte("hash-key"), []byte("recipient@example.test"))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("Hash() = %q, want 64 lowercase hex characters", got)
	}
	if got != Hash([]byte("hash-key"), []byte("recipient@example.test")) {
		t.Fatal("Hash() is not deterministic")
	}
	if got == Hash([]byte("other-key"), []byte("recipient@example.test")) {
		t.Fatal("Hash() ignored the key")
	}
}
