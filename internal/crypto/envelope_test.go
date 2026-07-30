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

func TestVersionedCryptoSelectsKeyByID(t *testing.T) {
	keys := map[string][]byte{
		"v1": bytes.Repeat([]byte{1}, 32),
		"v2": bytes.Repeat([]byte{2}, 32),
	}
	context := []byte("message-id:payload")
	plaintext := []byte("secret")

	ciphertext, err := EncryptWithKeyID(keys, "v1", context, plaintext)
	if err != nil {
		t.Fatalf("EncryptWithKeyID() error = %v", err)
	}
	decoded, err := DecryptWithKeyID(keys, "v1", context, ciphertext)
	if err != nil {
		t.Fatalf("DecryptWithKeyID() error = %v", err)
	}
	if !bytes.Equal(decoded, plaintext) {
		t.Fatalf("DecryptWithKeyID() = %q, want %q", decoded, plaintext)
	}
	if _, err := DecryptWithKeyID(keys, "v2", context, ciphertext); err == nil {
		t.Fatal("DecryptWithKeyID() error = nil, want wrong key rejected")
	}

	got, err := HashWithKeyID(keys, "v1", []byte("recipient@example.test"))
	if err != nil {
		t.Fatalf("HashWithKeyID() error = %v", err)
	}
	if got != Hash(keys["v1"], []byte("recipient@example.test")) {
		t.Fatalf("HashWithKeyID() = %q, want legacy Hash result", got)
	}
}

func TestVersionedCryptoRejectsUnknownKeyID(t *testing.T) {
	keys := map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}

	if _, err := EncryptWithKeyID(keys, "missing", []byte("context"), []byte("secret")); err == nil {
		t.Fatal("EncryptWithKeyID() error = nil, want unknown key rejected")
	}
	if _, err := DecryptWithKeyID(keys, "", []byte("context"), []byte("ciphertext")); err == nil {
		t.Fatal("DecryptWithKeyID() error = nil, want empty key ID rejected")
	}
	if _, err := HashWithKeyID(keys, "missing", []byte("value")); err == nil {
		t.Fatal("HashWithKeyID() error = nil, want unknown key rejected")
	}
}
