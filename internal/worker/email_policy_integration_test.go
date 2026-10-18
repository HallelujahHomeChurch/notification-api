//go:build integration

package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
)

func emailClaim(t *testing.T, db *sql.DB, template string) claim {
	t.Helper()
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	result, err := (postgresStore{db: db}).claim(context.Background(), id, time.Minute)
	if err != nil || !result.Claimed {
		t.Fatalf("claim: %#v %v", result, err)
	}
	result.Claim.TemplateID = template
	if _, err := db.Exec(`UPDATE notification_messages SET email_history_complete=true WHERE id=$1`, result.Claim.MessageID); err != nil {
		t.Fatal(err)
	}
	return result.Claim
}

func TestEmailBudgetSerializesReplicasAndReservesTransactionalCapacity(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 10, PerHour: 10, ReserveMinute: 2, ReserveHour: 2}}
	claims := make([]claim, 12)
	for i := range claims {
		claims[i] = emailClaim(t, db, "engagement.newsletter")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for _, c := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			delay, err := p.begin(context.Background(), c)
			if err != nil {
				t.Error(err)
			}
			if delay == 0 {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("unpaced concurrent sends: %d", admitted)
	}
	// Fill the bulk hourly allowance, leaving the transactional reserve unused.
	if _, err := db.Exec(`UPDATE notification_email_attempts SET started_at=now()-interval '2 minutes'; UPDATE notification_email_budget SET next_send_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		c := emailClaim(t, db, "engagement.newsletter")
		if delay, err := p.begin(context.Background(), c); err != nil || delay != 0 {
			t.Fatalf("bulk slot: %s %v", delay, err)
		}
		if _, err := db.Exec(`UPDATE notification_email_attempts SET started_at=now()-interval '2 minutes'; UPDATE notification_email_budget SET next_send_at=now()-interval '1 second'`); err != nil {
			t.Fatal(err)
		}
	}
	if delay, err := p.begin(context.Background(), emailClaim(t, db, "engagement.newsletter")); err != nil || delay < 50*time.Minute {
		t.Fatalf("hourly bulk ceiling: %s %v", delay, err)
	}
	if delay, err := p.begin(context.Background(), emailClaim(t, db, "account.verify-email")); err != nil || delay != 0 {
		t.Fatalf("transactional reserve unavailable: %s %v", delay, err)
	}
}

func TestEmailHistorySafetyFailsClosedAndRetainsUnknown(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if delay, err := p.begin(context.Background(), c); err != nil || delay != 0 {
		t.Fatalf("begin: %s %v", delay, err)
	}
	if err := p.finish(context.Background(), c, &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "mail", SMTPCode: 451}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_deliveries SET status='dead_lettered' WHERE message_id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET status='dead_lettered' WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	message, err := store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
	if err != nil || !message.SafeToRetry {
		t.Fatalf("definitive rejection not retryable: %#v %v", message, err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET email_history_complete=false WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	message, err = store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
	if err != nil || message.SafeToRetry {
		t.Fatalf("legacy history trusted: %#v %v", message, err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET email_history_complete=true WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_email_attempts SET outcome='unknown' WHERE delivery_id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	message, err = store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
	if err != nil || message.SafeToRetry {
		t.Fatalf("unknown history trusted: %#v %v", message, err)
	}
	c.Attempt++
	if _, err := p.begin(context.Background(), c); !errors.Is(err, errEmailAcceptanceUnknown) {
		t.Fatalf("campaign uncertainty not fenced: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM notification_messages WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("attempt retention cascade: %d %v", count, err)
	}
}

func TestEmailQuotaCooldownIsShared(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := p.begin(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(context.Background(), c, &providers.ProviderError{Kind: providers.ErrorRateLimited, Operation: "mail", SMTPCode: 451}); err != nil {
		t.Fatal(err)
	}
	other := &emailPolicy{db: db, limits: p.limits}
	if delay, err := other.begin(context.Background(), emailClaim(t, db, "account.verify-email")); err != nil || delay < 50*time.Second {
		t.Fatalf("cooldown ignored by another replica: %s %v", delay, err)
	}
}

func TestEmailAcceptedAttemptCannotBeSentAgainAfterReceiptWriteFailure(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := p.begin(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	c.Attempt++
	if _, err := p.begin(context.Background(), c); !errors.Is(err, errEmailAlreadyAccepted) {
		t.Fatalf("accepted email could be resent: %v", err)
	}
}

func TestEmailEvidenceRejectsExpiredPurgedAndIncompleteHistory(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{{"expired", `UPDATE notification_messages SET expires_at=now()-interval '1 second' WHERE id=$1`}, {"purged", `UPDATE notification_messages SET payload_purged_at=now(),payload_ciphertext=''::bytea WHERE id=$1`}, {"incomplete", `UPDATE notification_email_attempts SET outcome='started',finished_at=NULL WHERE delivery_id=(SELECT id FROM notification_deliveries WHERE message_id=$1)`}, {"accepted", `UPDATE notification_email_attempts SET outcome='accepted' WHERE delivery_id=(SELECT id FROM notification_deliveries WHERE message_id=$1)`}} {
		t.Run(tc.name, func(t *testing.T) {
			db := workerTestDatabase(t)
			p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
			c := emailClaim(t, db, "engagement.newsletter")
			if _, err := p.begin(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if err := p.finish(context.Background(), c, &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "mail", SMTPCode: 451}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE notification_deliveries SET status='dead_lettered' WHERE message_id=$1`, c.MessageID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE notification_messages SET status='dead_lettered' WHERE id=$1`, c.MessageID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tc.sql, c.MessageID); err != nil {
				t.Fatal(err)
			}
			result, err := store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
			if err != nil || result.SafeToRetry {
				t.Fatalf("unsafe history admitted: %#v %v", result, err)
			}
		})
	}
}

func TestEmailCampaignMissingPriorAttemptIsFenced(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	c.Attempt = 2
	if _, err := p.begin(context.Background(), c); !errors.Is(err, errEmailAcceptanceUnknown) {
		t.Fatalf("missing historical acceptance allowed: %v", err)
	}
	var complete bool
	if err := db.QueryRow(`SELECT email_history_complete FROM notification_messages WHERE id=$1`, c.MessageID).Scan(&complete); err != nil || complete {
		t.Fatalf("history fence not persisted: %v %v", complete, err)
	}
}

func TestEmailRetrySafetyDetectsUnrecordedLegacyClaims(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := p.begin(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(context.Background(), c, &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "mail", SMTPCode: 451}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET status='dead_lettered' WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_deliveries SET status='dead_lettered',attempt_count=2 WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	m, err := store.New(db, testKey).Get(context.Background(), "account-api", c.MessageID)
	if err != nil || m.SafeToRetry {
		t.Fatalf("unrecorded legacy claim trusted: %#v %v", m, err)
	}
}

func TestEmailAcceptedReceiptRecoversBeforeExpiryAndEligibility(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			db := workerTestDatabase(t)
			p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
			c := emailClaim(t, db, "engagement.newsletter")
			if _, err := p.begin(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if err := p.finish(context.Background(), c, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE notification_deliveries SET status='queued',lease_expires_at=NULL WHERE id=$1`, c.DeliveryID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE notification_messages SET expires_at=CASE WHEN $2 THEN now()-interval '1 second' ELSE now()+interval '1 hour' END,eligibility_campaign_id=$1,eligibility_recipient_id=$1 WHERE id=$1`, c.MessageID, expired); err != nil {
				t.Fatal(err)
			}
			provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
				t.Error("accepted email was resent")
				return providers.ProviderReceipt{}, nil
			}}
			eligibility := &fakeEligibilityChecker{allowed: false}
			message := &fakeMessage{id: c.DeliveryID}
			w := New(db, provider, testKey).WithEmailLimits(db, p.limits).WithEligibilityChecker(eligibility)
			if err := w.Process(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM notification_messages WHERE id=$1`, c.MessageID).Scan(&status); err != nil || status != "sent" {
				t.Fatalf("accepted receipt lost: %s %v", status, err)
			}
			if provider.calls != 0 || eligibility.calls != 0 || message.completed != 1 {
				t.Fatalf("recovery did not bypass send gates: calls=%d eligibility=%d complete=%d", provider.calls, eligibility.calls, message.completed)
			}
		})
	}
}

func TestEmailNoSendObservationDoesNotBecomeUnknownOnRetry(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if err := p.observe(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	c.Attempt++
	if err := p.observe(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if delay, err := p.begin(context.Background(), c); err != nil || delay != 0 {
		t.Fatalf("known no-send claim blocked: %s %v", delay, err)
	}
}

func TestEmailAcceptedEvidenceSurvivesOutboxExpiry(t *testing.T) {
	db := workerTestDatabase(t)
	p := &emailPolicy{db: db, limits: EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20}}
	c := emailClaim(t, db, "engagement.newsletter")
	if _, err := p.begin(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_deliveries SET status='dead_lettered',last_error_code='expired',lease_expires_at=NULL WHERE id=$1`, c.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET status='dead_lettered',terminal_at=now(),expires_at=now()-interval '1 second' WHERE id=$1`, c.MessageID); err != nil {
		t.Fatal(err)
	}
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("accepted email resent")
		return providers.ProviderReceipt{}, nil
	}}
	if err := New(db, provider, testKey).WithEmailLimits(db, p.limits).Process(context.Background(), &fakeMessage{id: c.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM notification_messages WHERE id=$1`, c.MessageID).Scan(&status); err != nil || status != "sent" {
		t.Fatalf("accepted evidence overwritten by outbox expiry: %s %v", status, err)
	}
}
