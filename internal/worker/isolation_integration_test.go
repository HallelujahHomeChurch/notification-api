//go:build integration

package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
)

var isolationLimits = EmailLimits{PerSecond: 5, ReserveSecond: 1, PerDay: 6000, ReserveDay: 1000}

func TestSESReplicasShareRollingRateReservations(t *testing.T) {
	db := workerTestDatabase(t)
	db.SetMaxOpenConns(5)
	policies := []*emailPolicy{{db: db, provider: "ses", limits: isolationLimits}, {db: db, provider: "ses", limits: isolationLimits}}
	claims := make([]claim, 20)
	for i := range claims {
		claims[i] = emailClaim(t, db, "engagement.newsletter")
		if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, claims[i].DeliveryID); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range claims {
		wg.Go(func() {
			<-start
			if _, err := policies[i%2].begin(context.Background(), c); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	var admitted, violations int
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_attempts WHERE outcome='started'`).Scan(&admitted); err != nil || admitted == 0 {
		t.Fatalf("admitted=%d: %v", admitted, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_attempts a WHERE a.outcome='started' AND (SELECT count(*) FROM notification_email_attempts b WHERE b.outcome='started' AND b.started_at>a.started_at-interval '1 second' AND b.started_at<=a.started_at)>4`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("replicas overran bulk reserve: violations=%d %v", violations, err)
	}
}

func TestSESRollingDayProtectsTransactionalReserve(t *testing.T) {
	db := workerTestDatabase(t)
	seed := emailClaim(t, db, "engagement.newsletter")
	if _, err := db.Exec(`INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider,started_at,finished_at) SELECT $1,n,true,CASE WHEN n%2=0 THEN 'unknown' ELSE 'rejected' END,'ses',clock_timestamp()-interval '1 minute',clock_timestamp() FROM generate_series(1,5000) n`, seed.DeliveryID); err != nil {
		t.Fatal(err)
	}
	smtp := emailClaim(t, db, "account.verify-email")
	if _, err := db.Exec(`INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider,started_at,finished_at) SELECT $1,n,false,'rejected','smtp',clock_timestamp(),clock_timestamp() FROM generate_series(1,20) n`, smtp.DeliveryID); err != nil {
		t.Fatal(err)
	}
	p := &emailPolicy{db: db, provider: "ses", limits: isolationLimits}
	next := func(template string) claim {
		c := emailClaim(t, db, template)
		if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, c.DeliveryID); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if delay, err := p.begin(context.Background(), next("engagement.newsletter")); err != nil || delay < 23*time.Hour {
		t.Fatalf("bulk consumed daily reserve: delay=%s %v", delay, err)
	}
	if delay, err := p.begin(context.Background(), next("account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("transactional reserve unavailable: delay=%s %v", delay, err)
	}
	if _, err := db.Exec(`INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider,started_at,finished_at) SELECT $1,n+5000,false,'rejected','ses',clock_timestamp()-interval '1 minute',clock_timestamp() FROM generate_series(1,999) n`, seed.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if delay, err := p.begin(context.Background(), next("account.verify-email")); err != nil || delay < 23*time.Hour {
		t.Fatalf("total daily quota bypassed: delay=%s %v", delay, err)
	}
}

type isolationProvider struct {
	family string
	calls  atomic.Int32
	send   func(context.Context) (providers.ProviderReceipt, error)
}

func (p *isolationProvider) Family() string { return p.family }
func (p *isolationProvider) Send(ctx context.Context, _ providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	p.calls.Add(1)
	return p.send(ctx)
}

func TestRollbackToSMTPPreservesPinnedAcceptanceAndDoesNotReplay(t *testing.T) {
	for _, outcome := range []string{"accepted", "unknown", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			db := workerTestDatabase(t)
			c := emailClaim(t, db, "account.verify-email")
			if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, c.DeliveryID); err != nil {
				t.Fatal(err)
			}
			p := &emailPolicy{db: db, provider: "ses", limits: isolationLimits}
			if _, err := p.begin(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			var providerErr error
			receipt := providers.ProviderReceipt{Provider: "ses"}
			if outcome == "accepted" {
				receipt.ProviderMessageID, receipt.AcceptedAt = "original-ses-receipt", time.Now()
			}
			if outcome == "unknown" {
				providerErr = &providers.ProviderError{Kind: providers.ErrorAcceptanceUnknown, Operation: "message"}
			}
			if outcome == "rejected" {
				providerErr = &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "message"}
			}
			if err := p.finish(context.Background(), c, receipt, providerErr); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE notification_deliveries SET status='queued',lease_expires_at=NULL,next_attempt_at=clock_timestamp() WHERE id=$1`, c.DeliveryID); err != nil {
				t.Fatal(err)
			}
			smtp := &isolationProvider{family: "smtp", send: func(context.Context) (providers.ProviderReceipt, error) {
				return providers.ProviderReceipt{Provider: "smtp", ProviderMessageID: "new-smtp-receipt", AcceptedAt: time.Now()}, nil
			}}
			w := New(db, smtp, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20})
			if err := w.Process(context.Background(), &fakeMessage{id: c.DeliveryID}); err != nil {
				t.Fatal(err)
			}
			var provider, status string
			if err := db.QueryRow(`SELECT provider,status FROM notification_deliveries WHERE id=$1`, c.DeliveryID).Scan(&provider, &status); err != nil {
				t.Fatal(err)
			}
			if smtp.calls.Load() != 0 || provider != "ses" {
				t.Fatal("rollback replayed or repinned an existing SES delivery")
			}
			if outcome == "accepted" && status != "sent" {
				t.Fatalf("accepted receipt lost: %s", status)
			}
			if outcome == "accepted" {
				var id string
				if err := db.QueryRow(`SELECT provider_message_id FROM notification_deliveries WHERE id=$1`, c.DeliveryID).Scan(&id); err != nil || id != "original-ses-receipt" {
					t.Fatalf("real SES receipt lost: %s %v", id, err)
				}
			}
			message, err := store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
			if err != nil || message.SafeToRetry != (outcome == "rejected") {
				t.Fatalf("unsafe rollback retry evidence: outcome=%s safe=%v %v", outcome, message.SafeToRetry, err)
			}
			if outcome != "accepted" && status != "dead_lettered" {
				t.Fatalf("mismatched provider not isolated: %s", status)
			}
			if _, err := db.Exec(`UPDATE notification_email_budget SET next_send_at='1970-01-01'`); err != nil {
				t.Fatal(err)
			}
			fresh := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
			for range 2 {
				if err := w.Process(context.Background(), &fakeMessage{id: fresh}); err != nil {
					t.Fatal(err)
				}
			}
			if smtp.calls.Load() != 1 {
				t.Fatalf("fresh SMTP intent calls=%d", smtp.calls.Load())
			}
		})
	}
}

func TestFakeSESReceiptWriteFailureRecoversWithoutResending(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE FUNCTION fail_test_receipt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.status='sent' THEN RAISE EXCEPTION 'injected receipt write failure'; END IF; RETURN NEW; END$$;
CREATE TRIGGER fail_test_receipt BEFORE UPDATE ON notification_deliveries FOR EACH ROW EXECUTE FUNCTION fail_test_receipt()`); err != nil {
		t.Fatal(err)
	}
	provider := &isolationProvider{family: "ses", send: func(context.Context) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{Provider: "ses", ProviderMessageID: "retained-ses-receipt", AcceptedAt: time.Now()}, nil
	}}
	w := New(db, provider, testKey).WithEmailLimits(db, isolationLimits)
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err == nil {
		t.Fatal("receipt fault was not exercised")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_test_receipt ON notification_deliveries`); err != nil {
		t.Fatal(err)
	}
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	var status, receipt string
	if err := db.QueryRow(`SELECT status,provider_message_id FROM notification_deliveries WHERE id=$1`, id).Scan(&status, &receipt); err != nil || status != "sent" || receipt != "retained-ses-receipt" || provider.calls.Load() != 1 {
		t.Fatalf("receipt recovery: status=%s receipt=%s calls=%d err=%v", status, receipt, provider.calls.Load(), err)
	}
}

func TestInflightSESCallMustDrainBeforeRollback(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	ses := &isolationProvider{family: "ses", send: func(context.Context) (providers.ProviderReceipt, error) {
		close(started)
		<-release
		return providers.ProviderReceipt{Provider: "ses", ProviderMessageID: "late-accepted-ses", AcceptedAt: time.Now()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- New(db, ses, testKey).WithEmailLimits(db, isolationLimits).Process(ctx, &fakeMessage{id: id})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SES fake did not start")
	}
	cancel()
	smtp := &isolationProvider{family: "smtp", send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Error("inflight SES delivery replayed through SMTP")
		return providers.ProviderReceipt{}, nil
	}}
	w := New(db, smtp, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100})
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err == nil {
		t.Error("delivery fence did not hold while old call could still complete")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old worker did not drain")
	}
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	if ses.calls.Load() != 1 || smtp.calls.Load() != 0 {
		t.Fatal("rollback duplicated an accepted send")
	}
}

func TestPostgresClaimPreservesProviderExpiry(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	var expiry time.Time
	if err := db.QueryRow(`UPDATE notification_messages SET expires_at=clock_timestamp()+interval '30 seconds' WHERE id=(SELECT message_id FROM notification_deliveries WHERE id=$1) RETURNING expires_at`, id).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	message := &fakeMessage{id: id}
	w := New(db, deadlineProvider{expiry, t}, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100})
	if err := w.Process(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if message.completed != 1 {
		t.Fatalf("provider expiry path did not complete: %+v", message)
	}
}

func TestEmailExpiryIsRecheckedAfterRendering(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	provider := &isolationProvider{family: "smtp", send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Error("expired mail reached provider")
		return providers.ProviderReceipt{}, nil
	}}
	w := New(db, provider, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100})
	w.resolveTemplate = func(id string, version int, channel string) (templates.Definition, error) {
		if _, err := db.Exec(`UPDATE notification_messages SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
			return templates.Definition{}, err
		}
		return templates.ResolveVersion(id, version, channel)
	}
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	var code string
	if err := db.QueryRow(`SELECT last_error_code FROM notification_deliveries WHERE id=$1`, id).Scan(&code); err != nil || code != "expired" {
		t.Fatalf("expiry status=%s: %v", code, err)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("expired mail sent")
	}
}

func TestSESQuotaValidationRejectsMixedOrInvalidWindows(t *testing.T) {
	for _, limits := range []EmailLimits{{PerSecond: 5, PerDay: 6000, PerMinute: 20}, {PerSecond: 0, PerDay: 6000}, {PerSecond: 5, PerDay: 6000, ReserveSecond: 5}, {PerSecond: 5, PerDay: 6000, ReserveDay: 6000}} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("unsafe limits accepted: %+v", limits)
		}
	}
	if err := isolationLimits.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeSES429SchedulesDurableRetryWithoutGeneralFailureCharge(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.Exec(`UPDATE notification_deliveries SET provider='ses' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	provider := &isolationProvider{family: "ses", send: func(context.Context) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorRateLimited, Operation: "message", HTTPStatus: 429, RetryAfter: time.Minute}
	}}
	w := New(db, provider, testKey).WithEmailLimits(db, isolationLimits)
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	var status string
	var failures, pending int
	if err := db.QueryRow(`SELECT status,failure_count FROM notification_deliveries WHERE id=$1`, id).Scan(&status, &failures); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notification_outbox WHERE delivery_id=$1 AND status='pending'`, id).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || failures != 0 || pending != 1 || provider.calls.Load() != 1 {
		t.Fatalf("unsafe throttle handling: status=%s failures=%d pending=%d calls=%d", status, failures, pending, provider.calls.Load())
	}
	if err := w.Process(context.Background(), &fakeMessage{id: id}); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 {
		t.Fatal("broker duplicate bypassed retry time")
	}
}
