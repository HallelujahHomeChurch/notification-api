package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestAbsoluteExpiryCanonicalCompatibility(t *testing.T) {
	request := validRequest()
	canonical, err := canonicalRequest(request, 2)
	if err != nil {
		t.Fatal(err)
	}
	const legacy = `{"templateId":"account.verify-email","templateVersion":2,"channel":"email","target":{"type":"email","address":"user@example.com"},"locale":"zh-Hant","payload":{"verifyUrl":"https://account.alive.org.tw/verify-email?token=opaque-token"},"resource":{"type":"account","id":"user-1"}}`
	if string(canonical) != legacy {
		t.Fatalf("legacy canonical changed: %s", canonical)
	}
	repo := &memoryRepository{}
	svc := New(repo, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	if _, err := svc.Send(context.Background(), "account-api", "old-ttl", request); err != nil {
		t.Fatal(err)
	}
	if repo.creates[0].ExpiresAfter != 24*time.Hour {
		t.Fatal("legacy TTL changed")
	}
	for _, stamp := range []string{"2026-10-16T20:00:00+08:00", "2026-10-16T12:00:00Z"} {
		if err := json.Unmarshal([]byte(`{"expiresAt":"`+stamp+`"}`), &request); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Send(context.Background(), "account-api", "absolute-utc", request); err != nil {
			t.Fatal(err)
		}
	}
	if repo.creates[1].RequestHash != repo.creates[2].RequestHash || repo.creates[1].RequestHash == repo.creates[0].RequestHash {
		t.Fatal("supplied deadline must affect hash with UTC equivalence")
	}
}

func TestEmailCorrectionVerificationRequiresDeadline(t *testing.T) {
	request := validRequest()
	request.TemplateID = "account.email-correction-verify"
	request.SubjectAccountID = "bf9c8d16-0c10-4841-aabd-ae308b8dd3ac"
	request.Resource.Type, request.Resource.ID = "email_correction", "3f6b02b2-58ef-4337-ad61-96bdfb588a65"
	request.Payload["verifyUrl"] = "https://account.alive.org.tw/data-requests/verify-email#token=opaque-token"
	repo := &memoryRepository{}
	svc := New(repo, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	if _, err := svc.Send(context.Background(), "account-api", "missing-deadline", request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing expiry error %v", err)
	}
	if len(repo.creates) != 0 {
		t.Fatal("missing deadline persisted")
	}
	if err := json.Unmarshal([]byte(`{"expiresAt":"2026-10-16T12:00:00Z"}`), &request); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(context.Background(), "account-api", "with-deadline", request); err != nil {
		t.Fatal(err)
	}
}

func TestAbsoluteExpiryChangeConflictsAndPersistsOriginalUTC(t *testing.T) {
	repo := &memoryRepository{}
	svc := New(repo, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey})
	request := validRequest()
	original := time.Date(2026, 10, 16, 20, 0, 0, 123000, time.FixedZone("test", 8*60*60))
	request.ExpiresAt = &original
	if _, err := svc.Send(context.Background(), "account-api", "absolute-conflict", request); err != nil {
		t.Fatal(err)
	}
	if got := repo.creates[0].ExpiresAt; got == nil || !got.Equal(original) || got.Location() != time.UTC {
		t.Fatalf("stored deadline %v", got)
	}
	changed := original.Add(time.Second)
	request.ExpiresAt = &changed
	if _, err := svc.Send(context.Background(), "account-api", "absolute-conflict", request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("deadline conflict %v", err)
	}
}
