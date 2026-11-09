//go:build integration

package worker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

func TestGraphForbiddenDurablyHaltsSubmission(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	first := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 100}}
	c := graphBudgetClaim(t, first, "account.verify-email")
	if delay, err := first.begin(ctx, c); err != nil || delay != 0 {
		t.Fatalf("initial reservation = %s, %v", delay, err)
	}
	forbidden := &providers.ProviderError{Kind: providers.ErrorPermanent, Operation: "auth", HTTPStatus: http.StatusForbidden}
	if err := first.finish(ctx, c, providers.ProviderReceipt{}, forbidden); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_graph_email_budget SET next_send_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	restarted := &emailPolicy{db: db, provider: "graph", limits: first.limits}
	if delay, err := restarted.begin(ctx, graphBudgetClaim(t, restarted, "account.verify-email")); err != nil || delay < time.Minute {
		t.Fatalf("restarted Graph admitted after configuration halt: %s, %v", delay, err)
	}
	legacy := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100}}
	if delay, err := legacy.begin(ctx, emailClaim(t, db, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("SMTP affected by Graph halt: %s, %v", delay, err)
	}
}

const fixtureGraphMailboxID = "0b3e6b27-bb63-441b-8a1c-214827219289"

type preparedGraphFixture struct {
	mailbox string
	prepare func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error)
}

func (*preparedGraphFixture) Family() string { return "graph" }

func (f *preparedGraphFixture) SenderMailboxID() string {
	if f.mailbox != "" {
		return f.mailbox
	}
	return fixtureGraphMailboxID
}

func (*preparedGraphFixture) Send(context.Context, providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	panic("worker called Graph without preparing the request")
}

func (f *preparedGraphFixture) Prepare(ctx context.Context, payload providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
	return f.prepare(ctx, payload)
}

func TestGraphWorkerChecksPermitAfterPreparation(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET provider='graph',graph_mailbox_id=$2 WHERE id=$1`, id, fixtureGraphMailboxID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f := &preparedGraphFixture{prepare: func(ctx context.Context, _ providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
		var outcome string
		if err := db.QueryRowContext(ctx, `SELECT outcome FROM notification_email_attempts WHERE delivery_id=$1`, id).Scan(&outcome); err != nil || outcome != "deferred" {
			t.Fatalf("preparation already consumed a permit: %q %v", outcome, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return func(context.Context) (providers.ProviderReceipt, error) {
			calls++
			return providers.ProviderReceipt{Provider: "graph", AcceptedAt: time.Now()}, nil
		}, nil
	}}
	instance := New(db, f, testKey).WithEmailLimits(db, EmailLimits{GraphDailyBudget: 20})
	if err := instance.Process(ctx, &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := db.QueryRowContext(ctx, `SELECT outcome FROM notification_email_attempts WHERE delivery_id=$1`, id).Scan(&outcome); err != nil || outcome != "deferred" || calls != 0 {
		t.Fatalf("expired Graph send calls=%d attempt=%q error=%v", calls, outcome, err)
	}
}

func TestGraphPreparationFailureDoesNotConsumeSubmission(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET provider='graph',graph_mailbox_id=$2 WHERE id=$1`, id, fixtureGraphMailboxID); err != nil {
		t.Fatal(err)
	}
	f := &preparedGraphFixture{prepare: func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
		return nil, &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "auth"}
	}}
	instance := New(db, f, testKey).WithEmailLimits(db, EmailLimits{GraphDailyBudget: 1})
	if err := instance.Process(ctx, &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	var outcome, state string
	if err := db.QueryRowContext(ctx, `SELECT a.outcome,d.status FROM notification_email_attempts a JOIN notification_deliveries d ON d.id=a.delivery_id WHERE d.id=$1`, id).Scan(&outcome, &state); err != nil || outcome != "deferred" || state != statusQueued {
		t.Fatalf("preparation error state=%q attempt=%q error=%v", state, outcome, err)
	}
	other := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 1}}
	if delay, err := other.begin(ctx, graphBudgetClaim(t, other, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("unused Graph quota unavailable: %s, %v", delay, err)
	}
}
