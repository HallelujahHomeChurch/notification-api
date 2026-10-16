//go:build integration

package worker

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/HallelujahHomeChurch/notification-api/internal/accountcleanup"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/outbox"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/google/uuid"
)

func correctionService(db *sql.DB) *service.Service {
	return service.New(store.New(db, bytes.Repeat([]byte{2}, 32)), service.Config{DataEncryptionKey: bytes.Repeat([]byte{1}, 32), HashKey: bytes.Repeat([]byte{2}, 32)})
}

func correctionIntent(deadline *time.Time) contracts.SendRequest {
	return contracts.SendRequest{TemplateID: "account.email-correction-verify", Channel: "email", Locale: "en", Target: contracts.Target{Type: "email", Address: "new@example.test"}, Payload: map[string]string{"verifyUrl": "https://account.alive.org.tw/data-requests/verify-email#token=synthetic-token"}, Resource: contracts.Resource{Type: "email_correction", ID: uuid.NewString()}, SubjectAccountID: uuid.NewString(), ExpiresAt: deadline}
}

func correctionDelivery(t *testing.T, db *sql.DB, messageID string) (string, time.Time) {
	t.Helper()
	var deliveryID string
	var deadline time.Time
	if err := db.QueryRow(`SELECT d.id,m.expires_at FROM notification_messages m JOIN notification_deliveries d ON d.message_id=m.id WHERE m.id=$1`, messageID).Scan(&deliveryID, &deadline); err != nil {
		t.Fatal(err)
	}
	return deliveryID, deadline
}

func TestPostgresEmailCorrectionAbsoluteExpiryCapsWithoutChangingLegacyTTL(t *testing.T) {
	db := workerTestDatabase(t)
	svc := correctionService(db)
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		legacy    bool
	}{{"delayed-enqueue", 5 * time.Minute, false}, {"already-expired", -time.Minute, false}, {"template-cap", 24 * time.Hour, false}, {"legacy-omitted", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var before time.Time
			if err := db.QueryRow(`SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			original := before.Add(tc.remaining)
			request := correctionIntent(&original)
			wantTTL := 30 * time.Minute
			if tc.legacy {
				request.TemplateID = "account.verify-email"
				request.ExpiresAt = nil
				request.Payload["verifyUrl"] = "https://account.alive.org.tw/verify-email?token=synthetic"
				wantTTL = 24 * time.Hour
			}
			result, err := svc.Send(context.Background(), "account-api", uuid.NewString(), request)
			if err != nil {
				t.Fatal(err)
			}
			_, stored := correctionDelivery(t, db, result.MessageID)
			if !tc.legacy && tc.remaining <= wantTTL {
				if !stored.Equal(original) {
					t.Fatalf("original %v stored %v", original, stored)
				}
			} else if stored.Before(before.Add(wantTTL)) || stored.After(before.Add(wantTTL+5*time.Second)) {
				t.Fatalf("stored TTL %v, want %v", stored.Sub(before), wantTTL)
			}
		})
	}
}

func TestPostgresEmailCorrectionAcceptedReplayAfterExpiryAndNoProviderSend(t *testing.T) {
	db := workerTestDatabase(t)
	svc := correctionService(db)
	var deadline time.Time
	if err := db.QueryRow(`SELECT clock_timestamp()+interval '300 milliseconds'`).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	request := correctionIntent(&deadline)
	idempotencyKey := uuid.NewString()
	first, err := svc.Send(context.Background(), "account-api", idempotencyKey, request)
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, stored := correctionDelivery(t, db, first.MessageID)
	if _, err := db.Exec(`SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, deadline); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Send(context.Background(), "account-api", idempotencyKey, request)
	if err != nil || !replay.Replayed || replay.MessageID != first.MessageID {
		t.Fatalf("replay %#v err %v", replay, err)
	}
	_, replayedDeadline := correctionDelivery(t, db, replay.MessageID)
	if !replayedDeadline.Equal(stored) {
		t.Fatal("replay extended original expiry")
	}
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("expired provider called")
		return providers.ProviderReceipt{}, nil
	}}
	worker := New(db, provider, bytes.Repeat([]byte{1}, 32))
	broker := &fakeMessage{id: deliveryID}
	if err := worker.Process(context.Background(), broker); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 || broker.deadLettered != 1 {
		t.Fatalf("provider calls %d dead letters %d", provider.calls, broker.deadLettered)
	}
	// The accepted receipt remains replayable after terminal expiry settlement too.
	replay, err = svc.Send(context.Background(), "account-api", idempotencyKey, request)
	if err != nil || !replay.Replayed || replay.MessageID != first.MessageID || replay.Status != contracts.MessageStatusDeadLettered {
		t.Fatalf("terminal replay %#v err %v", replay, err)
	}
}

type correctionPublisher struct{ calls int }

func (p *correctionPublisher) Publish(context.Context, string, string) error { p.calls++; return nil }

func TestPostgresEmailCorrectionExpiredNewIntentNeverPublishes(t *testing.T) {
	db := workerTestDatabase(t)
	svc := correctionService(db)
	deadline := time.Now().UTC().Add(-time.Minute)
	result, err := svc.Send(context.Background(), "account-api", uuid.NewString(), correctionIntent(&deadline))
	if err != nil {
		t.Fatal(err)
	}
	publisher := &correctionPublisher{}
	worked, err := outbox.New(db, publisher).DispatchOne(context.Background())
	if err != nil || !worked || publisher.calls != 0 {
		t.Fatalf("dispatch worked %v calls %d err %v", worked, publisher.calls, err)
	}
	deliveryID, _ := correctionDelivery(t, db, result.MessageID)
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("expired provider called")
		return providers.ProviderReceipt{}, nil
	}}
	if err := New(db, provider, bytes.Repeat([]byte{1}, 32)).Process(context.Background(), &fakeMessage{id: deliveryID}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls %d", provider.calls)
	}
}

func TestPostgresEmailCorrectionRetryDoesNotExtendOriginalDeadline(t *testing.T) {
	db := workerTestDatabase(t)
	svc := correctionService(db)
	var deadline time.Time
	if err := db.QueryRow(`SELECT clock_timestamp()+interval '1 second'`).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Send(context.Background(), "account-api", uuid.NewString(), correctionIntent(&deadline))
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := correctionDelivery(t, db, result.MessageID)
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorTemporary}
	}}
	worker := New(db, provider, bytes.Repeat([]byte{1}, 32))
	worker.retryDelay = func(int) time.Duration { return 0 }
	if err := worker.Process(context.Background(), &fakeMessage{id: deliveryID}); err != nil {
		t.Fatal(err)
	}
	_, stored := correctionDelivery(t, db, result.MessageID)
	if !stored.Equal(deadline) || provider.calls != 1 {
		t.Fatalf("retry deadline %v original %v calls %d", stored, deadline, provider.calls)
	}
	if _, err := db.Exec(`SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, deadline); err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(context.Background(), &fakeMessage{id: deliveryID}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatal("retry reached provider after original deadline")
	}
	_, stored = correctionDelivery(t, db, result.MessageID)
	if !stored.Equal(deadline) {
		t.Fatal("retry settlement changed original deadline")
	}
}

func TestPostgresEmailCorrectionAdminNoticeUsesApplicantErasureFence(t *testing.T) {
	db := workerTestDatabase(t)
	svc := correctionService(db)
	request := correctionIntent(nil)
	request.TemplateID = "account.email-correction-admin-review"
	request.Target.Address = "administrator@example.test"
	request.Payload = map[string]string{"reviewUrl": "https://admin.alive.org.tw/data-requests/" + request.Resource.ID}
	first, err := svc.Send(context.Background(), "account-api", uuid.NewString(), request)
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := correctionDelivery(t, db, first.MessageID)
	cleanup := accountcleanup.New(db, map[string][]byte{"legacy-v1": bytes.Repeat([]byte{2}, 32)})
	result, err := cleanup.CleanupAccount(context.Background(), accountcleanup.AccountCleanupRequest{UserID: request.SubjectAccountID, Email: "applicant@example.test", IdempotencyKey: uuid.NewString()})
	if err != nil || result.RemainingCount != 0 {
		t.Fatalf("cleanup %#v err %v", result, err)
	}
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("erased applicant notice sent to admin")
		return providers.ProviderReceipt{}, nil
	}}
	if err := New(db, provider, bytes.Repeat([]byte{1}, 32)).Process(context.Background(), &fakeMessage{id: deliveryID}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls %d", provider.calls)
	}
	if _, err := svc.Send(context.Background(), "account-api", uuid.NewString(), request); !errors.Is(err, service.ErrInvalidRequest) {
		t.Fatalf("erased applicant new intent: %v", err)
	}
}
