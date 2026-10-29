//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/outbox"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/HallelujahHomeChurch/notification-api/internal/worker"
	"github.com/google/uuid"
)

var pipelineKey = bytes.Repeat([]byte{1}, 32)

type pipelineProvider struct {
	family string
	send   func(context.Context, providers.DeliveryPayload) (providers.ProviderReceipt, error)
}

func (p pipelineProvider) Family() string { return p.family }
func (p pipelineProvider) Send(ctx context.Context, payload providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	return p.send(ctx, payload)
}

type pipelineMessage struct{ id string }

func (m pipelineMessage) DeliveryID() string                       { return m.id }
func (m pipelineMessage) Complete(context.Context) error           { return nil }
func (m pipelineMessage) DeadLetter(context.Context, string) error { return nil }

type pipelineBroker struct {
	ids   chan string
	block <-chan struct{}
}

func (b pipelineBroker) Publish(ctx context.Context, _, delivery string) error {
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case b.ids <- delivery:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type pipelineEligibility struct{}

func (pipelineEligibility) Check(context.Context, contracts.EligibilityRef) (bool, error) {
	return true, nil
}

func pipelineDatabase(t *testing.T) *sql.DB {
	t.Helper()
	var admin *sql.DB
	t.Cleanup(func() {
		if admin != nil {
			_ = admin.Close()
		}
	})
	var db *sql.DB
	admin, db = testDatabases(t)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueuePipeline(t *testing.T, db *sql.DB, template, label string, expiry *time.Time) (string, time.Time) {
	t.Helper()
	caller := "account-api"
	request := contracts.SendRequest{TemplateID: template, Channel: "email", Locale: "en", Target: contracts.Target{Type: "email", Address: "member-" + uuid.NewString() + "@example.test"}, Resource: contracts.Resource{Type: "test", ID: label}, ExpiresAt: expiry}
	switch template {
	case "engagement.newsletter":
		caller = "engagement-api"
		request.Payload = map[string]string{"subject": "Campaign-" + label, "body": "Synthetic campaign", "unsubscribeUrl": "https://www.alive.org.tw/unsubscribe#demo", "oneClickUnsubscribeUrl": "https://www.alive.org.tw/unsubscribe#demo"}
		request.EligibilityRef = &contracts.EligibilityRef{CampaignID: uuid.NewString(), RecipientID: uuid.NewString()}
	case "engagement.web-push":
		caller, request.Channel = "engagement-api", "web_push"
		request.Target = contracts.Target{Type: "web_push", Address: `{"endpoint":"https://push.example.test/demo","keys":{"p256dh":"demo","auth":"demo"}}`}
		request.Payload = map[string]string{"title": "Synthetic push", "body": "Synthetic", "clickBehavior": "dismiss"}
	default:
		request.Payload = map[string]string{"code": "000001", "provider": "google"}
	}
	svc := service.New(store.New(db, pipelineKey), service.Config{EmailProvider: "ses", DataEncryptionKey: pipelineKey, HashKey: pipelineKey})
	created := time.Now()
	result, err := svc.Send(context.Background(), caller, "pipeline:"+uuid.NewString(), request)
	if err != nil {
		t.Fatal(err)
	}
	var delivery string
	if err := db.QueryRow(`SELECT id FROM notification_deliveries WHERE message_id=$1`, result.MessageID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	return delivery, created
}

func startPipeline(t *testing.T, db *sql.DB, email providers.Provider, bulkBlock <-chan struct{}) (func(), <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	limits := worker.EmailLimits{PerSecond: 5, ReserveSecond: 1, PerDay: 6000, ReserveDay: 1000}
	push := pipelineProvider{family: "web_push", send: func(context.Context, providers.DeliveryPayload) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{Provider: "web_push", ProviderMessageID: "fake-push", AcceptedAt: time.Now()}, nil
	}}
	w := worker.NewWithProviders(db, map[string]providers.Provider{"email": email, "web_push": push}, map[string][]byte{"legacy-v1": pipelineKey}).WithEmailLimits(db, limits).WithEligibilityChecker(pipelineEligibility{})
	results := make(chan error, 6)
	failures := make(chan error, 6)
	report := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) {
			failures <- err
		}
		results <- err
	}
	for _, lane := range []queue.Lane{queue.LaneTransactional, queue.LaneBulk, queue.LaneWebPush} {
		broker := pipelineBroker{ids: make(chan string, 2)}
		if lane == queue.LaneBulk {
			broker.block = bulkBlock
		}
		go func() { report(outbox.NewForLane(db, broker, lane).Run(ctx)) }()
		go func() {
			for {
				select {
				case <-ctx.Done():
					report(ctx.Err())
					return
				case id := <-broker.ids:
					if err := w.Process(ctx, pipelineMessage{id: id}); err != nil {
						report(err)
						return
					}
				}
			}
		}()
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			for range 6 {
				select {
				case err := <-results:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("pipeline stopped: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Error("pipeline did not drain")
					return
				}
			}
		})
	}
	t.Cleanup(stop)
	return stop, failures
}

func TestFakeProviderPipelineSustainsCampaignAndTransactionalLatency(t *testing.T) {
	recipients, mode := 100, "pr"
	switch os.Getenv("HHC_EMAIL_PIPELINE_ACCEPTANCE") {
	case "":
	case "1000":
		recipients, mode = 1000, "acceptance"
	default:
		t.Fatal("HHC_EMAIL_PIPELINE_ACCEPTANCE must be unset or 1000")
	}
	t.Logf("pipeline_mode=%s recipients=%d", mode, recipients)
	db := pipelineDatabase(t)
	started := time.Now()
	for i := range recipients {
		enqueuePipeline(t, db, "engagement.newsletter", fmt.Sprint(i), nil)
	}
	type accepted struct {
		id   string
		at   time.Time
		bulk bool
	}
	events := make(chan accepted, 1100)
	provider := pipelineProvider{family: "ses", send: func(ctx context.Context, payload providers.DeliveryPayload) (providers.ProviderReceipt, error) {
		if err := ctx.Err(); err != nil {
			return providers.ProviderReceipt{}, err
		}
		now := time.Now()
		events <- accepted{payload.DeliveryID, now, strings.HasPrefix(payload.Subject, "Campaign-")}
		return providers.ProviderReceipt{Provider: "ses", ProviderMessageID: "fake-" + payload.DeliveryID, AcceptedAt: now}, nil
	}}
	stop, failures := startPipeline(t, db, provider, nil)
	var transaction string
	var generated time.Time
	bulk, txDone := 0, false
	window := started
	seen := map[string]bool{}
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	for bulk < recipients || !txDone {
		select {
		case err := <-failures:
			t.Fatalf("pipeline failed: %v", err)
		case event := <-events:
			if seen[event.id] {
				t.Fatalf("duplicate provider submission: %s", event.id)
			}
			seen[event.id] = true
			if event.bulk {
				bulk++
				if bulk == 10 {
					expiry := time.Now().Add(10 * time.Minute)
					transaction, generated = enqueuePipeline(t, db, "account.oauth-onboarding-code", "transaction", &expiry)
				}
				if bulk%100 == 0 {
					elapsed := event.at.Sub(window)
					if elapsed > time.Minute {
						t.Fatalf("campaign block %d took %s, want <=60s", bulk/100, elapsed)
					}
					t.Logf("campaign accepted=%d block_elapsed=%s", bulk, elapsed)
					window = event.at
				}
			} else {
				if event.id != transaction {
					t.Fatal("unexpected transaction")
				}
				latency := event.at.Sub(generated)
				if latency > 5*time.Second {
					t.Fatalf("transaction delayed by campaign: %s", latency)
				}
				t.Logf("transaction_submit_latency=%s", latency)
				txDone = true
			}
		case <-timer.C:
			t.Fatalf("pipeline timeout: bulk=%d transaction=%v", bulk, txDone)
		}
	}
	stop()
	var sent, violations int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE status='sent'`).Scan(&sent); err != nil || sent != recipients+1 {
		t.Fatalf("durable receipts=%d: %v", sent, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_attempts a WHERE a.provider='ses' AND a.outcome<>'deferred' AND ((SELECT count(*) FROM notification_email_attempts b WHERE b.provider='ses' AND b.outcome<>'deferred' AND b.started_at>a.started_at-interval '1 second' AND b.started_at<=a.started_at)>5 OR (SELECT count(*) FROM notification_email_attempts b WHERE b.provider='ses' AND b.bulk AND b.outcome<>'deferred' AND b.started_at>a.started_at-interval '1 second' AND b.started_at<=a.started_at)>4)`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("rolling rate violations=%d: %v", violations, err)
	}
	t.Logf("campaign_total=%d elapsed=%s receipts=%d", recipients, time.Since(started), sent)
}

func TestBlockedBulkPublisherDoesNotBlockAccountOrWebPush(t *testing.T) {
	db := pipelineDatabase(t)
	bulkBlock := make(chan struct{})
	enqueuePipeline(t, db, "engagement.newsletter", "blocked-bulk", nil)
	transaction, created := enqueuePipeline(t, db, "account.oauth-onboarding-code", "transaction", nil)
	push, _ := enqueuePipeline(t, db, "engagement.web-push", "push", nil)
	email := pipelineProvider{family: "ses", send: func(_ context.Context, p providers.DeliveryPayload) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{Provider: "ses", ProviderMessageID: "fake-" + p.DeliveryID, AcceptedAt: time.Now()}, nil
	}}
	_, failures := startPipeline(t, db, email, bulkBlock)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE id IN ($1,$2) AND status='sent'`, transaction, push).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			t.Logf("account_and_push_complete=%s", time.Since(created))
			return
		}
		select {
		case err := <-failures:
			t.Fatalf("pipeline failed: %v", err)
		case <-deadline.C:
			t.Fatal("bulk publisher blocked another lane")
		case <-ticker.C:
		}
	}
}
