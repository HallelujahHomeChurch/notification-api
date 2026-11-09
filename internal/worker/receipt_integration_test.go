//go:build integration

package worker

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
)

func TestGraphAcceptedWithoutMessageID(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := context.Background()
	p := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20, GraphDailyBudget: 100}}
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET provider='graph' WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := p.observe(ctx, c); err != nil {
		t.Fatal(err)
	}
	if delay, err := p.begin(ctx, c); err != nil || delay != 0 {
		t.Fatalf("begin = %s, %v", delay, err)
	}
	var family string
	if err := db.QueryRowContext(ctx, `SELECT provider FROM notification_email_attempts WHERE delivery_id=$1 AND attempt=$2`, c.DeliveryID, c.Attempt).Scan(&family); err != nil || family != "graph" {
		t.Fatalf("provider pin = %q, %v", family, err)
	}
	if _, err := p.acceptedReceipt(ctx, c); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("started attempt was treated as accepted: %v", err)
	}
	receipt := providers.ProviderReceipt{
		Provider: "graph", ProviderRequestID: "8442dc60-a99b-47bb-bcbe-adf912c83ec2",
		AcceptedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := p.finish(ctx, c, receipt, nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := p.acceptedReceipt(ctx, c)
	if err != nil || recovered.Provider != "graph" || recovered.ProviderMessageID != "" ||
		recovered.ProviderRequestID != receipt.ProviderRequestID || !recovered.AcceptedAt.Equal(receipt.AcceptedAt) {
		t.Fatalf("recovered receipt = %#v, %v", recovered, err)
	}
	provider := &fakeProvider{}
	if err := New(db, provider, testKey).WithEmailLimits(db, p.limits).Process(ctx, &fakeMessage{id: c.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	var storedID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT provider_message_id FROM notification_deliveries WHERE id=$1`, c.DeliveryID).Scan(&storedID); err != nil || storedID.Valid || provider.calls != 0 {
		t.Fatalf("recovery stored ID=%v provider calls=%d error=%v", storedID, provider.calls, err)
	}
	repo := store.New(db, testKey)
	legacy, err := repo.Get(ctx, "account-api", c.MessageID)
	if err != nil || legacy.Status != contracts.MessageStatusSent || legacy.SafeToRetry ||
		legacy.ProviderAcceptedAt != nil || legacy.DeliveryOutcome != "" {
		t.Fatalf("legacy status = %#v, %v", legacy, err)
	}
	repo.WithDeliveryEvidence(true)
	check := func(want contracts.DeliveryOutcome) {
		t.Helper()
		m, err := repo.Get(ctx, "account-api", c.MessageID)
		if err != nil || m.Status != contracts.MessageStatusSent || m.SafeToRetry ||
			m.ProviderAcceptedAt == nil || !m.ProviderAcceptedAt.Equal(receipt.AcceptedAt) || m.DeliveryOutcome != want {
			t.Fatalf("status = %#v, %v", m, err)
		}
	}
	check(contracts.DeliveryOutcomeUnknown)
	if _, err := db.ExecContext(ctx, `INSERT INTO notification_email_delivery_events(source,event_key,delivery_id,outcome,event_at) VALUES ('graph',repeat('a',64),$1,'bounced',clock_timestamp())`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	check(contracts.DeliveryOutcomeBounced)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
	check(contracts.DeliveryOutcomeBounced)
	if _, err := db.ExecContext(ctx, `DELETE FROM notification_messages WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_delivery_events WHERE delivery_id=$1`, c.DeliveryID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("feedback rows after parent deletion = %d, %v", events, err)
	}
}
