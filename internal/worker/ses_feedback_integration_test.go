//go:build integration

package worker

import (
	"context"
	"errors"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/sesfeedback"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestSESFeedbackSuppressesAcrossTemplatesAndPreservesLateDelivery(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := context.Background()
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1;`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET target_hash=$2 WHERE id=$1`, c.MessageID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	p := &emailPolicy{db: db, provider: "ses", limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	if _, err := p.begin(ctx, c); err != nil {
		t.Fatal(err)
	}
	receipt := providers.ProviderReceipt{Provider: "ses", ProviderMessageID: "ses-proof", AcceptedAt: time.Now().UTC()}
	if err := p.finish(ctx, c, receipt, nil); err != nil {
		t.Fatal(err)
	}
	store := sesfeedback.Store{DB: db}
	e := sesfeedback.Event{SNSMessageID: uuid.NewString(), MessageID: receipt.ProviderMessageID, DeliveryID: c.DeliveryID, Kind: "complaint", At: time.Now().UTC()}
	for range 2 {
		if err := store.Handle(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	conflict := e
	conflict.Kind = "hard_bounce"
	if err := store.Handle(ctx, conflict); !errors.Is(err, sesfeedback.ErrInvalidEvent) {
		t.Fatalf("conflicting duplicate accepted: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_feedback`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("non-idempotent feedback: %d %v", count, err)
	}
	e.SNSMessageID = uuid.NewString()
	e.Kind = "delivery"
	e.At = e.At.Add(-time.Minute)
	if err := store.Handle(ctx, e); err != nil {
		t.Fatal(err)
	}
	for _, template := range []string{"account.verify-email", "engagement.newsletter"} {
		next := emailClaim(t, db, template)
		// New active key retains the old-key alias: technical blocks survive rotation.
		if _, err := db.Exec(`UPDATE notification_messages SET hash_key_id='v2',target_hash=$2,target_hashes=jsonb_build_object('legacy-v1',$3::text) WHERE id=$1`, next.MessageID, strings.Repeat("b", 64), strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
		other := &emailPolicy{db: db, limits: p.limits}
		if _, err := other.begin(ctx, next); !errors.Is(err, errEmailSuppressed) {
			t.Fatalf("%s bypassed technical block: %v", template, err)
		}
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM notification_email_suppressions`).Scan(&reason); err != nil || reason != "complaint" {
		t.Fatalf("late delivery removed complaint: %s %v", reason, err)
	}
}

func TestSESFeedbackReconcilesUnknownWithoutResendingAndRejectsSMTP(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := context.Background()
	c := emailClaim(t, db, "engagement.newsletter")
	p := &emailPolicy{db: db, provider: "ses", limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	if _, err := p.begin(ctx, c); !errors.Is(err, errEmailProviderMismatch) {
		t.Fatalf("unreviewed provider change allowed: %v", err)
	}
	e := sesfeedback.Event{SNSMessageID: uuid.NewString(), MessageID: "unknown-ses-receipt", DeliveryID: c.DeliveryID, Kind: "delivery", At: time.Now().UTC()}
	store := sesfeedback.Store{DB: db}
	if err := store.Handle(ctx, e); !errors.Is(err, sesfeedback.ErrUnmatchedEvent) {
		t.Fatalf("SMTP event accepted: %v", err)
	}
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.begin(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(ctx, c, providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorAcceptanceUnknown, Operation: "data"}); err != nil {
		t.Fatal(err)
	}
	if err := (postgresStore{db: db}).markFailed(ctx, c, "acceptance_unknown"); err != nil {
		t.Fatal(err)
	}
	if err := store.Handle(ctx, e); err != nil {
		t.Fatal(err)
	}
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("accepted SES email resent")
		return providers.ProviderReceipt{}, nil
	}}
	if err := New(db, provider, testKey).WithEmailLimits(db, p.limits).Process(ctx, &fakeMessage{id: c.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	var state, id string
	if err := db.QueryRow(`SELECT status,provider_message_id FROM notification_deliveries WHERE id=$1`, c.DeliveryID).Scan(&state, &id); err != nil || state != "sent" || id != e.MessageID {
		t.Fatalf("reconciliation: %s %s %v", state, id, err)
	}
}

func TestSESFeedbackSoftBounceDoesNotSuppressAndHardBounceDoes(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := context.Background()
	c := emailClaim(t, db, "account.verify-email")
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	p := &emailPolicy{db: db, provider: "ses", limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	if _, err := p.begin(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET target_hash=$2,target_hashes=jsonb_build_object('older',$3::text) WHERE id=$1`, c.MessageID, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	s := sesfeedback.Store{DB: db}
	e := sesfeedback.Event{SNSMessageID: uuid.NewString(), MessageID: "ses-soft", DeliveryID: c.DeliveryID, Kind: "soft_bounce", At: time.Now().UTC()}
	if err := s.Handle(ctx, e); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_suppressions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("temporary failure blocked address: %d %v", n, err)
	}
	e.SNSMessageID = uuid.NewString()
	e.Kind = "hard_bounce"
	if err := s.Handle(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_suppressions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("hash aliases were not blocked: %d %v", n, err)
	}
	wrong := e
	wrong.SNSMessageID = uuid.NewString()
	wrong.MessageID = "unrelated-provider-id"
	if err := s.Handle(ctx, wrong); !errors.Is(err, sesfeedback.ErrUnmatchedEvent) {
		t.Fatalf("unrelated receipt accepted: %v", err)
	}
}

func TestSESFeedbackDoesNotHoldPoolWaitingForSender(t *testing.T) {
	db := workerTestDatabase(t)
	db.SetMaxOpenConns(2)
	ctx := context.Background()
	c := emailClaim(t, db, "account.verify-email")
	if _, err := db.Exec(`INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider) VALUES($1,1,false,'started','ses')`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	lock := "notification-dsr-delivery:" + c.DeliveryID
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, lock); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lock)
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	e := sesfeedback.Event{SNSMessageID: uuid.NewString(), DeliveryID: c.DeliveryID, MessageID: "in-flight", Kind: "send", At: time.Now().UTC()}
	if err := (sesfeedback.Store{DB: db}).Handle(deadline, e); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-flight event held the pool: %v", err)
	}
	if _, err := db.ExecContext(deadline, `SELECT 1`); err != nil {
		t.Fatalf("sender cannot finish: %v", err)
	}
}
