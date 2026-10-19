package service

import (
	"context"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/store"
)

func TestOAuthResendAdmissionPreservesOtherLimits(t *testing.T) {
	limits := []store.RateLimit{{Window: 15 * time.Minute, Maximum: 1}, {Window: 24 * time.Hour, Maximum: 5}, {Window: 24 * time.Hour, Maximum: 1000, TemplateWide: true}}
	repo := &memoryRepository{}
	svc := New(repo, Config{DataEncryptionKey: testEncryptionKey, HashKey: testHashKey, RateLimits: limits})
	request := validRequest()
	request.TemplateID = "account.oauth-onboarding-code"
	request.Payload = map[string]string{"code": "123456", "provider": "google"}
	expiry := time.Now().Add(20 * time.Minute).UTC()
	request.ExpiresAt = &expiry
	request.Payload["expiresAt"] = expiry.Format(time.RFC3339Nano)
	if _, err := svc.Send(context.Background(), "account-api", "oauth-resend", request); err != nil {
		t.Fatal(err)
	}
	got := repo.creates[0]
	if got.RateLimits[0].Window != 3*time.Minute || got.RateLimits[0].Maximum != 1 {
		t.Fatalf("cooldown = %#v", got.RateLimits)
	}
	for i := 1; i < len(limits); i++ {
		if got.RateLimits[i] != limits[i] {
			t.Fatalf("other limit changed: %#v", got.RateLimits)
		}
	}
	if got.ExpiresAfter != 30*time.Minute || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiry) {
		t.Fatalf("expiry = %s, %v", got.ExpiresAfter, got.ExpiresAt)
	}
	if _, err := svc.Send(context.Background(), "account-api", "other-template", validRequest()); err != nil {
		t.Fatal(err)
	}
	if repo.creates[1].RateLimits[0].Window != 15*time.Minute || limits[0].Window != 15*time.Minute {
		t.Fatal("shared configuration mutated")
	}
}
