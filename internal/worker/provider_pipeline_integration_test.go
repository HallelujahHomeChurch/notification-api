//go:build integration

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/accountcleanup"
	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/httpapi"
	"github.com/HallelujahHomeChurch/notification-api/internal/outbox"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/google/uuid"
)

type pipelinePublisher func(context.Context, string, string) error

func (f pipelinePublisher) Publish(ctx context.Context, messageID, deliveryID string) error {
	return f(ctx, messageID, deliveryID)
}

func TestNotificationProviderNeutralPipeline(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	keys := map[string][]byte{"legacy-v1": testKey}
	cfg := service.Config{EmailProvider: "graph", GraphMailboxID: fixtureGraphMailboxID, DataEncryptionKey: testKey, HashKey: testKey}
	api := func(evidence bool) http.Handler {
		svc := service.New(store.New(db, testKey).WithDeliveryEvidence(evidence), cfg)
		return httpapi.New(svc, db, []string{"account-api", "engagement-api"}, false, accountcleanup.New(db, keys))
	}
	legacyAPI, evidenceAPI := api(false), api(true)
	request := func(h http.Handler, caller, method, path, key string, body any, wantStatus int) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(encoded)).WithContext(ctx)
		r.Header.Set("Dapr-Caller-App-Id", caller)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != wantStatus {
			t.Fatalf("%s %s status=%d, want %d: %s", method, path, w.Code, wantStatus, w.Body)
		}
		var envelope struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}

	var dispatched string
	dispatcher := outbox.New(db, pipelinePublisher(func(_ context.Context, _, id string) error {
		dispatched = id
		return nil
	}))
	dispatch := func() *fakeMessage {
		t.Helper()
		dispatched = ""
		if ok, err := dispatcher.DispatchOne(ctx); err != nil || !ok || dispatched == "" {
			t.Fatalf("outbox dispatch = %v, %v", ok, err)
		}
		return &fakeMessage{id: dispatched}
	}

	subject := uuid.NewString()
	target := "fixture@example.test"
	email := contracts.SendRequest{
		TemplateID: "account.verify-email", Channel: "email", Locale: "en",
		Target: contracts.Target{Type: "email", Address: target}, SubjectAccountID: subject,
		Resource: contracts.Resource{Type: "account", ID: subject},
		Payload:  map[string]string{"verifyUrl": "https://account.alive.org.tw/verify-email?token=fixture"},
	}
	created := request(legacyAPI, "account-api", http.MethodPost, "/priv/notifications/send", "fixture-graph", email, http.StatusAccepted)
	id, ok := created["messageId"].(string)
	if !ok || id == "" || created["status"] != "queued" {
		t.Fatalf("queued email = %#v", created)
	}
	replay := request(legacyAPI, "account-api", http.MethodPost, "/priv/notifications/send", "fixture-graph", email, http.StatusAccepted)
	if replay["messageId"] != id || replay["replayed"] != true {
		t.Fatalf("admission replay = %#v", replay)
	}

	alternate := cfg
	alternate.GraphMailboxID = uuid.NewString()
	changedConfigReplay, err := service.New(store.New(db, testKey), alternate).Send(ctx, "account-api", "fixture-graph", email)
	if err != nil || !changedConfigReplay.Replayed || changedConfigReplay.MessageID != id {
		t.Fatalf("mailbox change repinned an existing delivery: %#v, %v", changedConfigReplay, err)
	}
	message := dispatch()
	if ok, err := dispatcher.DispatchOne(ctx); err != nil || ok {
		t.Fatalf("idempotent replay created duplicate outbox work: %v, %v", ok, err)
	}

	graphCalls := 0
	graph := &preparedGraphFixture{prepare: func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
		return func(context.Context) (providers.ProviderReceipt, error) {
			graphCalls++
			return providers.ProviderReceipt{Provider: "graph", AcceptedAt: time.Now(), ProviderRequestID: uuid.NewString()}, nil
		}, nil
	}}
	push := &fakeProvider{receipt: providers.ProviderReceipt{Provider: "webpush", ProviderMessageID: "fixture-push"}}
	instance := NewWithProviders(db, map[string]providers.Provider{"email": graph, "web_push": push}, keys).
		WithEmailLimits(db, EmailLimits{GraphDailyBudget: 10})

	// The provider has accepted, but the final sent-state transaction fails once.
	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION fail_mark_sent() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic sent-state failure'; END $$;
		CREATE TRIGGER fail_mark_sent BEFORE UPDATE OF status ON notification_deliveries
		FOR EACH ROW WHEN (NEW.status='sent') EXECUTE FUNCTION fail_mark_sent()`); err != nil {
		t.Fatal(err)
	}
	if err := instance.Process(ctx, message); err == nil || graphCalls != 1 || message.completed != 0 {
		t.Fatalf("receipt failure sends=%d complete=%d error=%v", graphCalls, message.completed, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_mark_sent ON notification_deliveries`); err != nil {
		t.Fatal(err)
	}
	recoveryGraph := &preparedGraphFixture{
		mailbox: uuid.NewString(),
		prepare: func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
			t.Fatal("stored acceptance must recover before mailbox routing or credential preparation")
			return nil, nil
		},
	}
	recovery := NewWithProviders(db, map[string]providers.Provider{"email": recoveryGraph, "web_push": push}, keys).
		WithEmailLimits(db, EmailLimits{GraphDailyBudget: 10}).WithEmailPaused(true)
	if err := recovery.Process(ctx, message); err != nil || graphCalls != 1 || message.completed != 1 {
		t.Fatalf("receipt recovery sends=%d complete=%d error=%v", graphCalls, message.completed, err)
	}
	path := "/priv/notifications/" + id
	oldStatus := request(legacyAPI, "account-api", http.MethodGet, path, "", nil, http.StatusOK)
	newStatus := request(evidenceAPI, "account-api", http.MethodGet, path, "", nil, http.StatusOK)
	if oldStatus["status"] != "sent" || oldStatus["providerAcceptedAt"] != nil || oldStatus["safeToRetry"] != false ||
		newStatus["status"] != "sent" || newStatus["providerAcceptedAt"] == nil || newStatus["deliveryOutcome"] != "unknown" {
		t.Fatalf("status compatibility = %#v, %#v", oldStatus, newStatus)
	}
	request(legacyAPI, "engagement-api", http.MethodGet, path, "", nil, http.StatusNotFound)

	webPush := contracts.SendRequest{
		TemplateID: "engagement.web-push", Channel: "web_push", Locale: "en",
		Target:   contracts.Target{Type: "web_push", Address: `{"endpoint":"https://push.example.test/sub","keys":{"p256dh":"fixture","auth":"fixture"}}`},
		Resource: contracts.Resource{Type: "campaign", ID: uuid.NewString()},
		Payload:  map[string]string{"title": "Fixture", "body": "Fixture", "clickBehavior": "dismiss"},
	}
	request(legacyAPI, "engagement-api", http.MethodPost, "/priv/notifications/send", "fixture-push", webPush, http.StatusAccepted)
	pushMessage := dispatch()
	if err := instance.Process(ctx, pushMessage); err != nil || push.calls != 1 || pushMessage.completed != 1 || graphCalls != 1 {
		t.Fatalf("push separation: push=%d graph=%d error=%v", push.calls, graphCalls, err)
	}
	cleanup := request(legacyAPI, "account-api", http.MethodPost, "/priv/account-cleanup", "",
		accountcleanup.AccountCleanupRequest{UserID: subject, Email: target, IdempotencyKey: "erase-fixture"}, http.StatusOK)
	if cleanup["status"] != "completed" {
		t.Fatalf("cleanup = %#v", cleanup)
	}
	var encryptedBytes int
	if err := db.QueryRowContext(ctx, `SELECT octet_length(target_ciphertext)+octet_length(payload_ciphertext) FROM notification_messages WHERE id=$1`, id).Scan(&encryptedBytes); err != nil || encryptedBytes != 0 {
		t.Fatalf("cleanup ciphertext bytes=%d error=%v", encryptedBytes, err)
	}
}

func TestLegacyWorkerMustBeFencedFromGraphPins(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.ExecContext(t.Context(), `UPDATE notification_deliveries SET provider='graph',graph_mailbox_id=$2 WHERE id=$1`, id, fixtureGraphMailboxID); err != nil {
		t.Fatal(err)
	}
	smtp := &fakeProvider{}
	message := &fakeMessage{id: id}
	instance := New(db, smtp, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100})
	if err := instance.Process(t.Context(), message); err != nil || smtp.calls != 0 || message.deadLetterReason != "provider_mismatch" {
		t.Fatalf("legacy worker sent Graph pin or missed routing fence: calls=%d reason=%q err=%v", smtp.calls, message.deadLetterReason, err)
	}
}

func TestGraphWorkerRejectsDifferentPinnedMailbox(t *testing.T) {
	db := workerTestDatabase(t)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	if _, err := db.ExecContext(t.Context(), `UPDATE notification_deliveries SET provider='graph',graph_mailbox_id=$2 WHERE id=$1`, id, uuid.New()); err != nil {
		t.Fatal(err)
	}
	f := &preparedGraphFixture{prepare: func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
		t.Fatal("attempted preparation for a different pinned sender mailbox")
		return nil, nil
	}}
	message := &fakeMessage{id: id}
	if err := New(db, f, testKey).WithEmailLimits(db, EmailLimits{GraphDailyBudget: 10}).Process(t.Context(), message); err != nil || message.deadLetterReason != "provider_mismatch" {
		t.Fatalf("wrong mailbox reason=%q err=%v", message.deadLetterReason, err)
	}
}

func TestEmailPausePreservesQueuedWorkAndWebPush(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	cfg := service.Config{DataEncryptionKey: testKey, HashKey: testKey}
	legacy := service.New(store.New(db, testKey), cfg)
	cfg.EmailPaused = true
	paused := service.New(store.New(db, testKey), cfg)
	email := contracts.SendRequest{
		TemplateID: "account.verify-email", Channel: "email", Locale: "en",
		Target:   contracts.Target{Type: "email", Address: "fixture@example.test"},
		Resource: contracts.Resource{Type: "account", ID: uuid.NewString()},
		Payload:  map[string]string{"verifyUrl": "https://account.alive.org.tw/verify-email?token=fixture"},
	}
	if _, err := paused.Send(ctx, "account-api", "blocked-email", email); !errors.Is(err, service.ErrNotificationsDisabled) {
		t.Fatalf("paused email admission = %v", err)
	}
	if _, err := legacy.Send(ctx, "account-api", "brokered-email", email); err != nil {
		t.Fatal(err)
	}
	var published string
	publisher := pipelinePublisher(func(_ context.Context, _, id string) error { published = id; return nil })
	legacyDispatcher := outbox.New(db, publisher)
	if ok, err := legacyDispatcher.DispatchOne(ctx); !ok || err != nil || published == "" {
		t.Fatalf("already brokered email = %v %v %q", ok, err, published)
	}
	brokered := &fakeMessage{id: published}
	email.Target.Address = "queued@example.test"
	if _, err := legacy.Send(ctx, "account-api", "queued-email", email); err != nil {
		t.Fatal(err)
	}
	push := contracts.SendRequest{
		TemplateID: "engagement.web-push", Channel: "web_push", Locale: "en",
		Target:   contracts.Target{Type: "web_push", Address: `{"endpoint":"https://push.example.test/sub","keys":{"p256dh":"fixture","auth":"fixture"}}`},
		Resource: contracts.Resource{Type: "campaign", ID: uuid.NewString()},
		Payload:  map[string]string{"title": "Fixture", "body": "Fixture", "clickBehavior": "dismiss"},
	}
	if _, err := paused.Send(ctx, "engagement-api", "unpaused-push", push); err != nil {
		t.Fatal(err)
	}
	smtp, webPush := &fakeProvider{}, &fakeProvider{}
	keys := map[string][]byte{"legacy-v1": testKey}
	configured := map[string]providers.Provider{"email": smtp, "web_push": webPush}
	instance := NewWithProviders(db, configured, keys).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100}).WithEmailPaused(true)
	if err := instance.Process(ctx, brokered); err != nil || brokered.completed != 1 || brokered.deadLetterReason != "" || smtp.calls != 0 {
		t.Fatalf("paused brokered email = %#v; SMTP calls=%d; err=%v", brokered, smtp.calls, err)
	}
	var status string
	var failures, deferred int
	if err := db.QueryRowContext(ctx, `SELECT d.status,d.failure_count,count(a.*) FILTER (WHERE a.outcome='deferred') FROM notification_deliveries d LEFT JOIN notification_email_attempts a ON a.delivery_id=d.id WHERE d.id=$1 GROUP BY d.id`, brokered.id).Scan(&status, &failures, &deferred); err != nil || status != "queued" || failures != 0 || deferred != 1 {
		t.Fatalf("paused delivery = %s, failures=%d, deferred=%d, err=%v", status, failures, deferred, err)
	}
	pausedDispatcher := outbox.NewWithEmailPaused(db, publisher, true)
	published = ""
	if ok, err := pausedDispatcher.DispatchOne(ctx); !ok || err != nil || published == "" {
		t.Fatalf("paused outbox push = %v %v %q", ok, err, published)
	}
	message := &fakeMessage{id: published}
	if err := instance.Process(ctx, message); err != nil || message.completed != 1 || webPush.calls != 1 || smtp.calls != 0 {
		t.Fatalf("paused push = %#v; push=%d smtp=%d err=%v", message, webPush.calls, smtp.calls, err)
	}
	if ok, err := pausedDispatcher.DispatchOne(ctx); ok || err != nil {
		t.Fatalf("paused outbox dispatched email: %v %v", ok, err)
	}
	email.Target.Address = "expired@example.test"
	expired, err := legacy.Send(ctx, "account-api", "expired-email", email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE notification_messages SET expires_at=clock_timestamp() - interval '1 second' WHERE id=$1`, expired.MessageID); err != nil {
		t.Fatal(err)
	}
	published = ""
	if ok, err := pausedDispatcher.DispatchOne(ctx); !ok || err != nil || published != "" {
		t.Fatalf("paused outbox expiry = %v %v %q", ok, err, published)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM notification_messages WHERE id=$1`, expired.MessageID).Scan(&status); err != nil || status != "dead_lettered" {
		t.Fatalf("expired email status=%q err=%v", status, err)
	}
	if ok, err := legacyDispatcher.DispatchOne(ctx); !ok || err != nil || published == "" {
		t.Fatalf("resumed outbox email = %v %v %q", ok, err, published)
	}
	resumed := NewWithProviders(db, configured, keys).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100})
	if err := resumed.Process(ctx, &fakeMessage{id: published}); err != nil || smtp.calls != 1 {
		t.Fatalf("resumed email sends=%d err=%v", smtp.calls, err)
	}
}

func TestGraphWorkerRoutesByDurablePinWithLegacyEmailPresent(t *testing.T) {
	db := workerTestDatabase(t)
	ctx := t.Context()
	legacyID := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	graphID := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	bulkID := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	for _, id := range []string{graphID, bulkID} {
		if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET provider='graph',graph_mailbox_id=$2 WHERE id=$1`, id, fixtureGraphMailboxID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_messages SET template_id='engagement.newsletter',template_version=1 WHERE id=(SELECT message_id FROM notification_deliveries WHERE id=$1)`, bulkID); err != nil {
		t.Fatal(err)
	}
	graphCalls := 0
	graph := &preparedGraphFixture{prepare: func(context.Context, providers.DeliveryPayload) (func(context.Context) (providers.ProviderReceipt, error), error) {
		return func(context.Context) (providers.ProviderReceipt, error) {
			graphCalls++
			return providers.ProviderReceipt{Provider: "graph", AcceptedAt: time.Now()}, nil
		}, nil
	}}
	smtp := &fakeProvider{}
	worker := New(db, smtp, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100, GraphDailyBudget: 10}).WithGraphEmailProvider(graph)
	for _, id := range []string{graphID, legacyID, bulkID} {
		message := &fakeMessage{id: id}
		if err := worker.Process(ctx, message); err != nil || message.completed != 1 {
			t.Fatalf("delivery %q complete=%d err=%v", id, message.completed, err)
		}
	}
	if graphCalls != 1 || smtp.calls != 1 {
		t.Fatalf("pinned calls: Graph=%d SMTP=%d", graphCalls, smtp.calls)
	}
	var status string
	var failures int
	if err := db.QueryRowContext(ctx, `SELECT status,failure_count FROM notification_deliveries WHERE id=$1`, bulkID).Scan(&status, &failures); err != nil || status != "queued" || failures != 0 {
		t.Fatalf("Graph bulk status=%q failures=%d err=%v", status, failures, err)
	}
}
