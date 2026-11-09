//go:build integration

package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

func TestGraphBudgetReservesTransactionalCapacityAndKeepsACSWindow(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	limits := EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20, GraphDailyBudget: 200}
	graph := &emailPolicy{db: db, provider: "graph", limits: limits}
	campaign := graphBudgetClaim(t, graph, "engagement.newsletter")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider)
		SELECT $1,n,true,'rejected',clock_timestamp()-interval '10 seconds','graph' FROM generate_series(2,21) n`, campaign.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if delay, err := graph.begin(ctx, campaign); err != nil || delay < 45*time.Second {
		t.Fatalf("campaign delay = %s, %v", delay, err)
	}
	account := graphBudgetClaim(t, graph, "account.verify-email")
	if delay, err := graph.begin(ctx, account); err != nil || delay != 0 {
		t.Fatalf("transactional Graph reserve blocked: %s, %v", delay, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider)
		SELECT $1,n,false,'rejected',clock_timestamp()-interval '10 seconds','graph' FROM generate_series(2,5) n`, account.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_graph_email_budget SET next_send_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	if delay, err := graph.begin(ctx, graphBudgetClaim(t, graph, "account.verify-email")); err != nil || delay < 45*time.Second {
		t.Fatalf("transactional Graph exceeded total cap: %s, %v", delay, err)
	}
	acs := &emailPolicy{db: db, limits: limits}
	legacy := emailClaim(t, db, "account.verify-email")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider)
		SELECT $1,n,false,'rejected',clock_timestamp()-interval '2 minutes','smtp' FROM generate_series(2,101) n`, legacy.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if delay, err := acs.begin(ctx, legacy); err != nil || delay < 50*time.Minute {
		t.Fatalf("legacy hourly limit lost: %s, %v", delay, err)
	}
}

func TestGraphDailyBudgetCountsUnknownAndDoesNotBlockSMTP(t *testing.T) {
	db := workerTestDatabase(t)
	graph := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 1}}
	c := graphBudgetClaim(t, graph, "account.verify-email")
	if _, err := db.Exec(`
		INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider)
		VALUES ($1,2,false,'unknown',clock_timestamp()-interval '2 hours','graph')`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if delay, err := graph.begin(t.Context(), c); err != nil || delay < 21*time.Hour {
		t.Fatalf("daily Graph budget = %s, %v", delay, err)
	}
	acs := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	if delay, err := acs.begin(t.Context(), emailClaim(t, db, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("ACS was blocked by Graph: %s, %v", delay, err)
	}
}

func TestGraphFailedAttemptsKeepTheirPinnedBudget(t *testing.T) {
	for _, kind := range []providers.ErrorKind{providers.ErrorAcceptanceUnknown, providers.ErrorPermanent} {
		t.Run(string(kind), func(t *testing.T) {
			db := workerTestDatabase(t)
			ctx := t.Context()
			graph := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 1}}
			c := graphBudgetClaim(t, graph, "account.verify-email")
			if delay, err := graph.begin(ctx, c); err != nil || delay != 0 {
				t.Fatalf("initial reservation = %s, %v", delay, err)
			}
			untrustedReceipt := providers.ProviderReceipt{Provider: "smtp", ProviderMessageID: "invalid-receipt", AcceptedAt: time.Now()}
			if err := graph.finish(ctx, c, untrustedReceipt, &providers.ProviderError{Kind: kind, Operation: "data"}); err != nil {
				t.Fatal(err)
			}
			var family string
			var accepted bool
			if err := db.QueryRowContext(ctx, `SELECT provider,provider_message_id IS NOT NULL OR provider_accepted_at IS NOT NULL FROM notification_email_attempts WHERE delivery_id=$1`, c.DeliveryID).Scan(&family, &accepted); err != nil || family != "graph" || accepted {
				t.Fatalf("failed attempt pin=%q acceptance=%v error=%v", family, accepted, err)
			}
			if delay, err := graph.begin(ctx, graphBudgetClaim(t, graph, "account.verify-email")); err != nil || delay < 23*time.Hour {
				t.Fatalf("failed Graph attempt dropped out of its daily budget: %s, %v", delay, err)
			}
			acs := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 1, PerHour: 1}}
			if delay, err := acs.begin(ctx, emailClaim(t, db, "account.verify-email")); err != nil || delay != 0 {
				t.Fatalf("failed Graph attempt polluted ACS budget: %s, %v", delay, err)
			}
		})
	}
}

func TestGraphCannotRecordMismatchedAcceptedReceipt(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	graph := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 2}}
	c := graphBudgetClaim(t, graph, "account.verify-email")
	if delay, err := graph.begin(ctx, c); err != nil || delay != 0 {
		t.Fatalf("initial reservation = %s, %v", delay, err)
	}
	if err := graph.finish(ctx, c, providers.ProviderReceipt{Provider: "smtp", ProviderMessageID: "smtp-id"}, nil); err == nil {
		t.Fatal("accepted receipt from a different family")
	}
	var family, outcome string
	if err := db.QueryRowContext(ctx, `SELECT provider,outcome FROM notification_email_attempts WHERE delivery_id=$1`, c.DeliveryID).Scan(&family, &outcome); err != nil || family != "graph" || outcome != "started" {
		t.Fatalf("mismatched receipt overwrote original attempt: %q %q %v", family, outcome, err)
	}
}

func TestGraphRetryAfterDoesNotThrottleACS(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	graph := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 100}}
	c := graphBudgetClaim(t, graph, "account.verify-email")
	if delay, err := graph.begin(ctx, c); err != nil || delay != 0 {
		t.Fatalf("initial Graph reservation = %s, %v", delay, err)
	}
	throttle := &providers.ProviderError{Kind: providers.ErrorRateLimited, Operation: "data", RetryAfter: 2 * time.Minute}
	if err := graph.finish(ctx, c, providers.ProviderReceipt{}, throttle); err != nil {
		t.Fatal(err)
	}
	if delay, err := graph.begin(ctx, graphBudgetClaim(t, graph, "account.verify-email")); err != nil || delay < 110*time.Second {
		t.Fatalf("Graph Retry-After = %s, %v", delay, err)
	}
	acs := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	if delay, err := acs.begin(ctx, emailClaim(t, db, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("ACS inherited Graph cooldown: %s, %v", delay, err)
	}
}

func TestLostGraphWorkerLeavesBoundedInFlightHold(t *testing.T) {
	db := workerTestDatabase(t)
	first := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 10}}
	second := &emailPolicy{db: db, provider: "graph", limits: first.limits}
	if delay, err := first.begin(t.Context(), graphBudgetClaim(t, first, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("initial reservation = %s, %v", delay, err)
	}
	// No finish call represents a worker or database session lost after reserving.
	if delay, err := second.begin(t.Context(), graphBudgetClaim(t, second, "account.verify-email")); err != nil || delay < 40*time.Second || delay > 49*time.Second {
		t.Fatalf("durable in-flight hold = %s, %v", delay, err)
	}
}

func TestExpiredGraphPermitCannotSubmit(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	graph := &emailPolicy{db: db, provider: "graph", limits: EmailLimits{GraphDailyBudget: 10}}
	c := graphBudgetClaim(t, graph, "account.verify-email")
	if delay, err := graph.begin(ctx, c); err != nil || delay != 0 {
		t.Fatalf("reserve = %s, %v", delay, err)
	}
	if _, err := db.Exec(`UPDATE notification_email_attempts SET started_at=clock_timestamp()-interval '2 seconds' WHERE delivery_id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := graph.confirmGraphPermit(ctx, c); err != nil || allowed {
		t.Fatalf("expired permit confirmation = %v, %v", allowed, err)
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM notification_email_attempts WHERE delivery_id=$1`, c.DeliveryID).Scan(&outcome); err != nil || outcome != "deferred" {
		t.Fatalf("expired permit = %q, %v", outcome, err)
	}
	if delay, err := graph.begin(ctx, graphBudgetClaim(t, graph, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("known unsubmitted permit retained a hold: %s, %v", delay, err)
	}
}

func TestGraphWorkersShareOneInFlightFence(t *testing.T) {
	db := workerTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &graphIntegrationProvider{integrationProvider{send: func(ctx context.Context) (providers.ProviderReceipt, error) {
		first := false
		once.Do(func() {
			first = true
			close(started)
		})
		if !first {
			return providers.ProviderReceipt{Provider: "graph", AcceptedAt: time.Now()}, nil
		}
		select {
		case <-release:
			return providers.ProviderReceipt{Provider: "graph", AcceptedAt: time.Now()}, nil
		case <-ctx.Done():
			return providers.ProviderReceipt{}, ctx.Err()
		}
	}}}
	makeDelivery := func() string {
		t.Helper()
		id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
		if _, err := db.Exec(`UPDATE notification_deliveries SET provider='graph' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := makeDelivery(), makeDelivery()
	one, two := New(db, p, testKey), New(db, p, testKey)
	limits := EmailLimits{GraphDailyBudget: 20}
	one.WithEmailLimits(db, limits)
	two.WithEmailLimits(db, limits)
	done := make(chan error, 1)
	go func() { done <- one.Process(ctx, &fakeMessage{id: first}) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The advisory fence still applies if the durable hold's deadline has passed.
	if _, err := db.ExecContext(ctx, `UPDATE notification_graph_email_budget SET next_send_at='epoch'`); err != nil {
		t.Fatal(err)
	}
	if err := two.Process(ctx, &fakeMessage{id: second}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := db.QueryRow(`SELECT status,last_error_code FROM notification_deliveries WHERE id=$1`, second).Scan(&state, &reason); err != nil || state != statusQueued || reason != "rate_limited" {
		t.Fatalf("second delivery = %q %q, %v", state, reason, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || db.Stats().InUse != 0 {
		t.Fatalf("provider calls=%d, checked-out connections=%d", p.calls, db.Stats().InUse)
	}
}

type graphIntegrationProvider struct{ integrationProvider }

func (*graphIntegrationProvider) Family() string { return "graph" }

func graphBudgetClaim(t *testing.T, p *emailPolicy, template string) claim {
	t.Helper()
	c := emailClaim(t, p.db, template)
	if _, err := p.db.Exec(`UPDATE notification_deliveries SET provider='graph' WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	return c
}
