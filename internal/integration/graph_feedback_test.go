//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/graphfeedback"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/google/uuid"
)

func TestGraphFeedbackCheckpointIsAtomicAndReplaySafe(t *testing.T) {
	ctx := t.Context()
	admin, db := testDatabases(t)
	defer admin.Close()
	defer db.Close()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	mailbox, message, delivery := uuid.New(), uuid.New(), uuid.New()
	key := bytes.Repeat([]byte{1}, 32)
	targetHash := string(bytes.Repeat([]byte{'a'}, 64))
	if _, err := db.ExecContext(ctx, `INSERT INTO notification_messages
		(id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,hash_key_id,target_ciphertext,payload_ciphertext,resource_type,resource_id,status)
		VALUES ($1,'account-api',$1::uuid::text,'hash','account.verify-email',1,'email','email',$2,'hash-v1','\x01','\x02','account','fixture','sent')`, message, targetHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notification_deliveries
		(id,message_id,channel,provider,status,attempt_count,graph_mailbox_id) VALUES($1,$2,'email','graph','sent',1,$3)`, delivery, message, mailbox); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notification_email_attempts
		(delivery_id,attempt,bulk,outcome,provider,provider_accepted_at) VALUES($1,1,false,'started','graph',NULL)`, delivery); err != nil {
		t.Fatal(err)
	}
	s, err := graphfeedback.NewStore(db, mailbox, "cursor-v1", map[string][]byte{"cursor-v1": key})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.Cursor(ctx)
	if err != nil || initial.Value != "" {
		t.Fatalf("initial cursor = %q, %v", initial.Value, err)
	}
	event := graphfeedback.VerifiedEvent{
		MailboxID: mailbox, DeliveryID: delivery, Attempt: 1, SourceEventID: "verified-fixture-event",
		RecipientHashKeyID: "hash-v1", RecipientHash: targetHash, Outcome: contracts.DeliveryOutcomeBounced,
		OccurredAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if _, err := s.ApplyPage(ctx, initial, graphfeedback.Page{NextCursor: "pending", Events: []graphfeedback.VerifiedEvent{event}}); !errors.Is(err, graphfeedback.ErrReceiptPending) {
		t.Fatalf("fresh attempt advanced before acceptance: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_email_attempts SET outcome='accepted',provider_accepted_at=clock_timestamp() WHERE delivery_id=$1`, delivery); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ApplyPage(ctx, initial, graphfeedback.Page{NextCursor: "opaque-cursor-one", Events: []graphfeedback.VerifiedEvent{event}}); err != nil || result.Stored != 1 {
		t.Fatalf("first page = %#v, %v", result, err)
	}
	current, err := s.Cursor(ctx)
	if err != nil || current.Value != "opaque-cursor-one" {
		t.Fatalf("current cursor = %q, %v", current.Value, err)
	}
	var ciphertext []byte
	if err := db.QueryRowContext(ctx, `SELECT cursor_ciphertext FROM notification_email_feedback_cursors`).Scan(&ciphertext); err != nil ||
		len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(current.Value)) {
		t.Fatalf("cursor was not encrypted: %v", err)
	}
	if err := store.ValidateKeyReferences(ctx, db, map[string][]byte{"legacy-v1": key}, nil); err == nil || !strings.Contains(err.Error(), "cursor-v1") {
		t.Fatal("cursor encryption key can be retired while referenced")
	}
	if _, err := s.ApplyPage(ctx, initial, graphfeedback.Page{NextCursor: "stale"}); !errors.Is(err, graphfeedback.ErrCursorMoved) {
		t.Fatalf("stale cursor overwrite = %v", err)
	}

	fence, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Rollback()
	if _, err := fence.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+delivery.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPage(ctx, current, graphfeedback.Page{NextCursor: "busy", Events: []graphfeedback.VerifiedEvent{event}}); !errors.Is(err, graphfeedback.ErrDeliveryBusy) {
		t.Fatalf("feedback bypassed active delivery fence: %v", err)
	}
	if err := fence.Rollback(); err != nil {
		t.Fatal(err)
	}

	conflict := event
	conflict.Outcome = contracts.DeliveryOutcomeDelivered
	pending := event
	pending.SourceEventID = "first-item-before-conflict"
	if _, err := s.ApplyPage(ctx, current, graphfeedback.Page{NextCursor: "conflict", Events: []graphfeedback.VerifiedEvent{pending, conflict}}); !errors.Is(err, graphfeedback.ErrConflictingEvent) {
		t.Fatalf("conflicting page = %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_delivery_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed page persisted partial events: count=%d err=%v", count, err)
	}

	wrongRecipient := event
	wrongRecipient.SourceEventID = "wrong-recipient"
	wrongRecipient.RecipientHash = string(bytes.Repeat([]byte{'b'}, 64))
	wrongMailbox := event
	wrongMailbox.SourceEventID, wrongMailbox.MailboxID = "wrong-mailbox", uuid.New()
	wrongAttempt := event
	wrongAttempt.SourceEventID, wrongAttempt.Attempt = "wrong-attempt", 2
	if _, err := db.ExecContext(ctx, `INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider,started_at)
		VALUES($1,2,false,'unknown','graph',clock_timestamp()-interval '10 minutes')`, delivery); err != nil {
		t.Fatal(err)
	}
	readCalls := 0
	consumer := graphfeedback.Consumer{Store: s, ReadPage: func(_ context.Context, cursor string) (graphfeedback.Page, error) {
		readCalls++
		if readCalls == 1 && cursor == "opaque-cursor-one" {
			return graphfeedback.Page{}, graphfeedback.ErrCursorExpired
		}
		if readCalls != 2 || cursor != "" {
			t.Fatalf("unexpected cursor resync call %d", readCalls)
		}
		return graphfeedback.Page{NextCursor: "resynced", Events: []graphfeedback.VerifiedEvent{event, wrongRecipient, wrongMailbox, wrongAttempt}}, nil
	}}
	if result, err := consumer.Poll(ctx); err != nil || result.Stored != 0 || result.Unmatched != 3 {
		t.Fatalf("resync page = %#v, %v", result, err)
	}
	resynced, err := s.Cursor(ctx)
	if err != nil || resynced.Value != "resynced" {
		t.Fatalf("resynced cursor = %q, %v", resynced.Value, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET graph_mailbox_id=$2 WHERE id=$1`, delivery, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ApplyPage(ctx, resynced, graphfeedback.Page{NextCursor: "wrong-pin", Events: []graphfeedback.VerifiedEvent{pending}}); err != nil || result.Unmatched != 1 || result.Stored != 0 {
		t.Fatalf("accepted wrong durable mailbox: %#v, %v", result, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_deliveries SET graph_mailbox_id=$2 WHERE id=$1`, delivery, mailbox); err != nil {
		t.Fatal(err)
	}
	resynced, err = s.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_messages SET target_hash=repeat('0',64),target_hashes='{}'::jsonb WHERE id=$1`, message); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ApplyPage(ctx, resynced, graphfeedback.Page{NextCursor: "after-erasure", Events: []graphfeedback.VerifiedEvent{pending}}); err != nil || result.Unmatched != 1 || result.Stored != 0 {
		t.Fatalf("erased recipient accepted feedback: %#v, %v", result, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM notification_messages WHERE id=$1`, message); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_delivery_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan provider events count=%d err=%v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_suppressions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("source-only feedback wrote suppression: count=%d err=%v", count, err)
	}
}
