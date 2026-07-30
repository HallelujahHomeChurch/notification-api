package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
)

var (
	testEncryptionKey = bytes.Repeat([]byte{1}, 32)
	testHashKey       = bytes.Repeat([]byte{2}, 32)
)

type memoryRepository struct {
	messages map[string]store.Message
	creates  []store.CreateParams
	result   store.CreateResult
	err      error
}

func (r *memoryRepository) Create(_ context.Context, params store.CreateParams) (store.CreateResult, error) {
	r.creates = append(r.creates, params)
	if r.err != nil {
		return store.CreateResult{}, r.err
	}
	if r.result.RetryAfter > 0 || r.result.Conflict {
		return r.result, nil
	}
	if r.messages == nil {
		r.messages = make(map[string]store.Message)
	}
	key := params.Caller + "\x00" + params.IdempotencyKey
	if existing, ok := r.messages[key]; ok {
		if existing.RequestHash != params.RequestHashes[existing.HashKeyID] {
			return store.CreateResult{Conflict: true}, nil
		}
		return store.CreateResult{Message: existing, Replayed: true}, nil
	}
	message := store.Message{
		ID:              params.MessageID,
		Caller:          params.Caller,
		RequestHash:     params.RequestHash,
		HashKeyID:       params.HashKeyID,
		TemplateVersion: params.TemplateVersion,
		Status:          contracts.MessageStatusQueued,
	}
	r.messages[key] = message
	return store.CreateResult{Message: message}, nil
}

func (r *memoryRepository) Get(_ context.Context, caller, messageID string) (store.Message, error) {
	for _, message := range r.messages {
		if message.ID == messageID && message.Caller == caller {
			return message, nil
		}
	}
	return store.Message{}, sql.ErrNoRows
}

func TestSendCreatesEncryptedIntent(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{
		DataEncryptionKey: testEncryptionKey,
		HashKey:           testHashKey,
	})

	result, err := svc.Send(context.Background(), "account-api", "verify-user-1", validRequest())
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if result.Status != contracts.MessageStatusQueued || result.TemplateVersion != 1 || result.Replayed {
		t.Fatalf("Send() = %#v", result)
	}
	if len(repository.creates) != 1 {
		t.Fatalf("Create() calls = %d, want 1", len(repository.creates))
	}

	params := repository.creates[0]
	if bytes.Contains(params.TargetCiphertext, []byte("user@example.com")) {
		t.Fatal("target ciphertext contains plaintext email")
	}
	if bytes.Contains(params.PayloadCiphertext, []byte("opaque-token")) {
		t.Fatal("payload ciphertext contains plaintext token")
	}
	target, err := notificationcrypto.Decrypt(testEncryptionKey, []byte(result.MessageID+":target"), params.TargetCiphertext)
	if err != nil {
		t.Fatalf("Decrypt(target) error = %v", err)
	}
	if string(target) != "user@example.com" {
		t.Fatalf("decrypted target = %q", target)
	}
	payload, err := notificationcrypto.Decrypt(testEncryptionKey, []byte(result.MessageID+":payload"), params.PayloadCiphertext)
	if err != nil {
		t.Fatalf("Decrypt(payload) error = %v", err)
	}
	if string(payload) != `{"locale":"zh-Hant","fields":{"verifyUrl":"https://account.alive.org.tw/verify-email?token=opaque-token"}}` {
		t.Fatalf("decrypted payload = %s", payload)
	}
}

func TestSendPersistsActiveKeyIDsAndAllHashIdentities(t *testing.T) {
	encryptionKeys := map[string][]byte{
		"v1": bytes.Repeat([]byte{1}, 32),
		"v2": bytes.Repeat([]byte{3}, 32),
	}
	hashKeys := map[string][]byte{
		"v1": bytes.Repeat([]byte{2}, 32),
		"v2": bytes.Repeat([]byte{4}, 32),
	}
	repository := &memoryRepository{}
	svc := New(repository, Config{
		ActiveEncryptionKeyID: "v2",
		EncryptionKeys:        encryptionKeys,
		ActiveHashKeyID:       "v2",
		HashKeys:              hashKeys,
	})

	result, err := svc.Send(context.Background(), "account-api", "versioned", validRequest())
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	params := repository.creates[0]
	if params.EncryptionKeyID != "v2" || params.HashKeyID != "v2" {
		t.Fatalf("key IDs = %q/%q, want v2/v2", params.EncryptionKeyID, params.HashKeyID)
	}
	if len(params.RequestHashes) != 2 || len(params.TargetHashes) != 2 {
		t.Fatalf("hash identities = %#v/%#v", params.RequestHashes, params.TargetHashes)
	}
	if params.RequestHash != params.RequestHashes["v2"] ||
		params.TargetHash != params.TargetHashes["v2"] {
		t.Fatal("persisted hashes do not use active hash key")
	}
	if params.ExpiresAfter != 24*time.Hour {
		t.Fatalf("ExpiresAfter = %s, want 24h", params.ExpiresAfter)
	}
	if _, err := notificationcrypto.Decrypt(
		encryptionKeys["v2"],
		[]byte(result.MessageID+":payload"),
		params.PayloadCiphertext,
	); err != nil {
		t.Fatalf("active-key decrypt error = %v", err)
	}
}

func TestSendReplaysExistingRequestAcrossHashKeyRotation(t *testing.T) {
	repository := &memoryRepository{}
	keys := map[string][]byte{
		"v1": bytes.Repeat([]byte{2}, 32),
		"v2": bytes.Repeat([]byte{4}, 32),
	}
	firstService := New(repository, Config{
		ActiveEncryptionKeyID: "v1",
		EncryptionKeys:        map[string][]byte{"v1": testEncryptionKey},
		ActiveHashKeyID:       "v1",
		HashKeys:              keys,
	})
	first, err := firstService.Send(context.Background(), "account-api", "rotated", validRequest())
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	rotatedService := New(repository, Config{
		ActiveEncryptionKeyID: "v2",
		EncryptionKeys: map[string][]byte{
			"v1": testEncryptionKey,
			"v2": bytes.Repeat([]byte{3}, 32),
		},
		ActiveHashKeyID: "v2",
		HashKeys:        keys,
	})
	replay, err := rotatedService.Send(context.Background(), "account-api", "rotated", validRequest())
	if err != nil {
		t.Fatalf("rotated Send() error = %v", err)
	}
	if !replay.Replayed || replay.MessageID != first.MessageID {
		t.Fatalf("rotated Send() = %#v, want replay of %s", replay, first.MessageID)
	}
}

func TestSendCanonicalHashReplaysSameRequest(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	request := validRequest()

	first, err := svc.Send(context.Background(), "account-api", "same-key", request)
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	second, err := svc.Send(context.Background(), "account-api", "same-key", request)
	if err != nil {
		t.Fatalf("second Send() error = %v", err)
	}
	if second.MessageID != first.MessageID || !second.Replayed {
		t.Fatalf("second Send() = %#v, want replay of %s", second, first.MessageID)
	}
	if repository.creates[0].RequestHash != repository.creates[1].RequestHash {
		t.Fatal("canonical request hash changed for identical requests")
	}
}

func TestSendCanonicalHashIgnoresEmailCaseAndPayloadMapOrder(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	firstRequest := validRequest()
	firstRequest.Target.Address = " USER@Example.COM "
	firstRequest.Payload = map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque-token",
	}
	secondRequest := validRequest()

	first, err := svc.Send(context.Background(), "account-api", "canonical-key", firstRequest)
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	second, err := svc.Send(context.Background(), "account-api", "canonical-key", secondRequest)
	if err != nil {
		t.Fatalf("second Send() error = %v", err)
	}
	if second.MessageID != first.MessageID || !second.Replayed {
		t.Fatalf("second Send() = %#v, want canonical replay", second)
	}
}

func TestSendRejectsIdempotencyConflict(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	if _, err := svc.Send(context.Background(), "account-api", "same-key", validRequest()); err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	changed := validRequest()
	changed.Resource.ID = "different-user"

	_, err := svc.Send(context.Background(), "account-api", "same-key", changed)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("second Send() error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSendScopesIdempotencyKeyByCaller(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	request := validRequest()

	_, err := svc.Send(context.Background(), "account-api", "shared-key", request)
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	second, err := svc.Send(context.Background(), "other-api", "shared-key", request)
	if !errors.Is(err, ErrForbiddenTemplate) {
		t.Fatalf("other caller Send() error = %v, want ErrForbiddenTemplate", err)
	}
	if second.MessageID != "" {
		t.Fatalf("other caller Send() = %#v", second)
	}
}

func TestGetDoesNotRevealAnotherCallersMessage(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	created, err := svc.Send(context.Background(), "account-api", "status-key", validRequest())
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if _, err := svc.Get(context.Background(), "other-api", created.MessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestSendReturnsRateLimitRetryDuration(t *testing.T) {
	repository := &memoryRepository{
		result: store.CreateResult{RetryAfter: 37 * time.Second},
	}
	svc := New(repository, Config{
		DataEncryptionKey: testEncryptionKey,
		HashKey:           testHashKey,
		RateLimits:        []store.RateLimit{{Window: time.Minute, Maximum: 1}},
	})

	_, err := svc.Send(context.Background(), "account-api", "limited-key", validRequest())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Send() error = %v, want ErrRateLimited", err)
	}
	var rateLimitError *RateLimitError
	if !errors.As(err, &rateLimitError) || rateLimitError.RetryAfter != 37*time.Second {
		t.Fatalf("Send() error = %#v, want 37s retry duration", err)
	}
}

func TestSendDoesNotPersistWhenNotificationsDisabled(t *testing.T) {
	repository := &memoryRepository{}
	svc := New(repository, Config{
		DataEncryptionKey:     testEncryptionKey,
		HashKey:               testHashKey,
		NotificationsDisabled: true,
	})

	_, err := svc.Send(context.Background(), "account-api", "disabled-key", validRequest())
	if !errors.Is(err, ErrNotificationsDisabled) {
		t.Fatalf("Send() error = %v, want ErrNotificationsDisabled", err)
	}
	if len(repository.creates) != 0 {
		t.Fatalf("Create() calls = %d, want 0", len(repository.creates))
	}
}

func TestSendRejectsInvalidIdempotencyKey(t *testing.T) {
	svc := New(&memoryRepository{}, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	for _, key := range []string{"", "contains space", "line\nbreak"} {
		if _, err := svc.Send(context.Background(), "account-api", key, validRequest()); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Send(key=%q) error = %v, want ErrInvalidRequest", key, err)
		}
	}
}

func validRequest() contracts.SendRequest {
	return contracts.SendRequest{
		TemplateID: "account.verify-email",
		Channel:    "email",
		Target: contracts.Target{
			Type:    "email",
			Address: "user@example.com",
		},
		Locale: "zh-Hant",
		Payload: map[string]string{
			"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque-token",
		},
		Resource: contracts.Resource{
			Type: "account",
			ID:   "user-1",
		},
	}
}
