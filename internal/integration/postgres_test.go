//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/google/uuid"
)

func TestPostgresLedger(t *testing.T) {
	ctx := context.Background()
	admin, scoped := testDatabases(t)
	defer admin.Close()
	defer scoped.Close()

	if err := migrations.Run(ctx, scoped); err != nil {
		t.Fatalf("first migrations.Run() error = %v", err)
	}
	if err := migrations.Run(ctx, scoped); err != nil {
		t.Fatalf("second migrations.Run() error = %v", err)
	}

	testMigrationAdvisoryLock(t, ctx, scoped)
	testDurableNotificationService(t, ctx, scoped)
	testTemplateWideRateLimit(t, ctx, scoped)
	testConcurrentIdempotentReplay(t, ctx, scoped)
	testConcurrentIdempotencyConflict(t, ctx, scoped)

	if _, err := scoped.ExecContext(ctx, `UPDATE schema_migrations SET checksum='changed' WHERE version='sql/001_initial.sql'`); err != nil {
		t.Fatalf("corrupt migration checksum: %v", err)
	}
	if err := migrations.Run(ctx, scoped); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("migrations.Run() error = %v, want checksum mismatch", err)
	}

	testSkipLocked(t, ctx, scoped)
}

type sendOutcome struct {
	result service.Result
	err    error
}

func testTemplateWideRateLimit(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	hashKey := bytes.Repeat([]byte{3}, 32)
	svc := service.New(store.New(db, hashKey), service.Config{
		DataEncryptionKey: bytes.Repeat([]byte{1}, 32),
		HashKey:           hashKey,
		RateLimits: []store.RateLimit{
			{Window: time.Hour, Maximum: 1},
			{Window: 24 * time.Hour, Maximum: 1, TemplateWide: true},
		},
	})
	requests := []contracts.SendRequest{
		integrationRequest("template-a-"+suffix+"@example.com", "template-a-"+suffix),
		integrationRequest("template-b-"+suffix+"@example.com", "template-b-"+suffix),
	}
	keys := []string{"template-a-" + suffix, "template-b-" + suffix}
	rateRowsBefore, rateCountBefore := rateTotals(t, ctx, db)

	start := make(chan struct{})
	outcomes := make([]sendOutcome, len(requests))
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			outcomes[index].result, outcomes[index].err = svc.Send(
				ctx, "account-api", keys[index], requests[index],
			)
		}()
	}
	close(start)
	wait.Wait()

	accepted := -1
	limited := 0
	for index, outcome := range outcomes {
		switch {
		case outcome.err == nil:
			accepted = index
		case errors.Is(outcome.err, service.ErrRateLimited):
			limited++
		default:
			t.Fatalf("template-wide Send() error = %v", outcome.err)
		}
	}
	if accepted < 0 || limited != 1 {
		t.Fatalf("template-wide outcomes = %#v, want one accepted and one rate limited", outcomes)
	}

	replay, err := svc.Send(ctx, "account-api", keys[accepted], requests[accepted])
	if err != nil || !replay.Replayed {
		t.Fatalf("accepted replay = %#v, error = %v", replay, err)
	}
	rateRowsAfter, rateCountAfter := rateTotals(t, ctx, db)
	if rateRowsAfter != rateRowsBefore+2 || rateCountAfter != rateCountBefore+2 {
		t.Fatalf(
			"template-wide rate totals rows=%d->%d count=%d->%d, want two consumed buckets",
			rateRowsBefore,
			rateRowsAfter,
			rateCountBefore,
			rateCountAfter,
		)
	}

	var messages int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM notification_messages
		WHERE caller_app_id='account-api' AND idempotency_key IN ($1,$2)`,
		keys[0],
		keys[1],
	).Scan(&messages); err != nil {
		t.Fatalf("count template-wide messages: %v", err)
	}
	if messages != 1 {
		t.Fatalf("template-wide messages = %d, want 1", messages)
	}
}

func testConcurrentIdempotentReplay(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "concurrent-replay-" + suffix
	request := integrationRequest(
		"concurrent-"+suffix+"@example.com",
		"concurrent-replay-"+suffix,
	)
	hashKey := bytes.Repeat([]byte{2}, 32)
	svc := integrationService(db, hashKey)
	rateRowsBefore, rateCountBefore := rateTotals(t, ctx, db)

	outcomes := concurrentSends(t, ctx, db, hashKey, key, svc, request, request)
	var original, replay *service.Result
	for index := range outcomes {
		if outcomes[index].err != nil {
			t.Fatalf("concurrent replay Send() error = %v", outcomes[index].err)
		}
		if outcomes[index].result.Replayed {
			replay = &outcomes[index].result
		} else {
			original = &outcomes[index].result
		}
	}
	if original == nil || replay == nil || original.MessageID != replay.MessageID {
		t.Fatalf("concurrent replay outcomes = %#v", outcomes)
	}
	assertLedgerCounts(t, ctx, db, "account-api", key, 1, 1, 1)

	rateRowsAfter, rateCountAfter := rateTotals(t, ctx, db)
	if rateRowsAfter != rateRowsBefore+1 || rateCountAfter != rateCountBefore+1 {
		t.Fatalf(
			"rate totals rows=%d->%d count=%d->%d, want one consumed slot",
			rateRowsBefore,
			rateRowsAfter,
			rateCountBefore,
			rateCountAfter,
		)
	}
	if _, err := svc.Send(ctx, "account-api", key+"-limited", request); !errors.Is(err, service.ErrRateLimited) {
		t.Fatalf("post-replay Send() error = %v, want ErrRateLimited", err)
	}
	rateRowsFinal, rateCountFinal := rateTotals(t, ctx, db)
	if rateRowsFinal != rateRowsAfter || rateCountFinal != rateCountAfter {
		t.Fatalf(
			"rejected send changed rate totals rows=%d->%d count=%d->%d",
			rateRowsAfter,
			rateRowsFinal,
			rateCountAfter,
			rateCountFinal,
		)
	}
}

func testConcurrentIdempotencyConflict(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "concurrent-conflict-" + suffix
	first := integrationRequest(
		"conflict-"+suffix+"@example.com",
		"concurrent-conflict-a-"+suffix,
	)
	second := first
	second.Resource.ID = "concurrent-conflict-b-" + suffix
	hashKey := bytes.Repeat([]byte{2}, 32)
	svc := integrationService(db, hashKey)

	outcomes := concurrentSends(t, ctx, db, hashKey, key, svc, first, second)
	successes, conflicts := 0, 0
	for _, outcome := range outcomes {
		switch {
		case outcome.err == nil:
			successes++
			if outcome.result.Replayed {
				t.Fatalf("conflicting request replayed: %#v", outcome)
			}
		case errors.Is(outcome.err, service.ErrIdempotencyConflict):
			conflicts++
		default:
			t.Fatalf("concurrent conflict Send() error = %v", outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent conflict outcomes = %#v", outcomes)
	}
	assertLedgerCounts(t, ctx, db, "account-api", key, 1, 1, 1)
}

func concurrentSends(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	hashKey []byte,
	idempotencyKey string,
	svc *service.Service,
	requests ...contracts.SendRequest,
) []sendOutcome {
	t.Helper()
	blocker, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("idempotency blocker connection: %v", err)
	}
	defer blocker.Close()

	lockKey := notificationcrypto.Hash(
		hashKey,
		[]byte("account-api\x00"+idempotencyKey),
	)
	if _, err := blocker.ExecContext(
		ctx,
		`SELECT pg_advisory_lock(hashtextextended($1,0))`,
		lockKey,
	); err != nil {
		t.Fatalf("acquire idempotency blocker: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = blocker.ExecContext(
				context.Background(),
				`SELECT pg_advisory_unlock(hashtextextended($1,0))`,
				lockKey,
			)
		}
	}()

	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan sendOutcome, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			result, err := svc.Send(sendCtx, "account-api", idempotencyKey, request)
			results <- sendOutcome{result: result, err: err}
		}()
	}
	close(start)
	waitForAdvisoryWaiters(t, sendCtx, db, len(requests))

	if _, err := blocker.ExecContext(
		sendCtx,
		`SELECT pg_advisory_unlock(hashtextextended($1,0))`,
		lockKey,
	); err != nil {
		t.Fatalf("release idempotency blocker: %v", err)
	}
	locked = false

	outcomes := make([]sendOutcome, 0, len(requests))
	for range requests {
		select {
		case outcome := <-results:
			outcomes = append(outcomes, outcome)
		case <-sendCtx.Done():
			t.Fatalf("concurrent sends timed out: %v", sendCtx.Err())
		}
	}
	return outcomes
}

func waitForAdvisoryWaiters(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiters int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_locks
			WHERE locktype='advisory'
			  AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
			  AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatalf("count advisory waiters: %v", err)
		}
		if waiters >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d advisory waiters: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func integrationService(db *sql.DB, hashKey []byte) *service.Service {
	return service.New(store.New(db, hashKey), service.Config{
		DataEncryptionKey: bytes.Repeat([]byte{1}, 32),
		HashKey:           hashKey,
		RateLimits:        []store.RateLimit{{Window: time.Hour, Maximum: 1}},
	})
}

func integrationRequest(address, resourceID string) contracts.SendRequest {
	return contracts.SendRequest{
		TemplateID: "account.verify-email",
		Channel:    "email",
		Target:     contracts.Target{Type: "email", Address: address},
		Locale:     "en",
		Payload: map[string]string{
			"verifyUrl": "https://account.alive.org.tw/verify-email?token=" + resourceID,
		},
		Resource: contracts.Resource{Type: "account", ID: resourceID},
	}
}

func assertLedgerCounts(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	caller string,
	idempotencyKey string,
	wantMessages int,
	wantDeliveries int,
	wantOutbox int,
) {
	t.Helper()
	var messages, deliveries, outbox int
	if err := db.QueryRowContext(ctx, `
		SELECT count(DISTINCT m.id), count(DISTINCT d.id), count(DISTINCT o.id)
		FROM notification_messages m
		LEFT JOIN notification_deliveries d ON d.message_id=m.id
		LEFT JOIN notification_outbox o ON o.delivery_id=d.id
		WHERE m.caller_app_id=$1 AND m.idempotency_key=$2`,
		caller,
		idempotencyKey,
	).Scan(&messages, &deliveries, &outbox); err != nil {
		t.Fatalf("count concurrent ledger: %v", err)
	}
	if messages != wantMessages || deliveries != wantDeliveries || outbox != wantOutbox {
		t.Fatalf(
			"ledger counts messages=%d deliveries=%d outbox=%d, want %d/%d/%d",
			messages,
			deliveries,
			outbox,
			wantMessages,
			wantDeliveries,
			wantOutbox,
		)
	}
}

func rateTotals(t *testing.T, ctx context.Context, db *sql.DB) (int64, int64) {
	t.Helper()
	var rows, count int64
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(count),0)
		FROM notification_rate_limits`).Scan(&rows, &count); err != nil {
		t.Fatalf("read rate totals: %v", err)
	}
	return rows, count
}

func testDurableNotificationService(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	encryptionKey := bytes.Repeat([]byte{1}, 32)
	hashKey := bytes.Repeat([]byte{2}, 32)
	svc := service.New(store.New(db, hashKey), service.Config{
		DataEncryptionKey: encryptionKey,
		HashKey:           hashKey,
		RateLimits:        []store.RateLimit{{Window: time.Hour, Maximum: 1}},
	})
	request := contracts.SendRequest{
		TemplateID: "account.verify-email",
		Channel:    "email",
		Target:     contracts.Target{Type: "email", Address: "integration@example.com"},
		Locale:     "en",
		Payload: map[string]string{
			"verifyUrl": "https://account.alive.org.tw/verify-email?token=integration",
		},
		Resource: contracts.Resource{Type: "account", ID: "integration-user"},
	}

	first, err := svc.Send(ctx, "account-api", "integration-send", request)
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	replay, err := svc.Send(ctx, "account-api", "integration-send", request)
	if err != nil {
		t.Fatalf("replay Send() error = %v", err)
	}
	if replay.MessageID != first.MessageID || !replay.Replayed {
		t.Fatalf("replay Send() = %#v, first = %#v", replay, first)
	}

	otherCaller := store.New(db, hashKey)
	otherResult, err := otherCaller.Create(ctx, store.CreateParams{
		MessageID:         uuid.NewString(),
		DeliveryID:        uuid.NewString(),
		OutboxID:          uuid.NewString(),
		Caller:            "hhc-web-api",
		IdempotencyKey:    "integration-send",
		RequestHash:       "other-caller-hash",
		TemplateID:        "test",
		TemplateVersion:   1,
		Channel:           "email",
		TargetType:        "email",
		TargetHash:        "other-target-hash",
		TargetCiphertext:  []byte("ciphertext"),
		PayloadCiphertext: []byte("ciphertext"),
		ResourceType:      "test",
		ResourceID:        "other-caller",
		Provider:          "test",
	})
	if err != nil {
		t.Fatalf("different caller Create() error = %v", err)
	}
	if otherResult.Conflict || otherResult.Replayed {
		t.Fatalf("different caller Create() = %#v", otherResult)
	}

	changed := request
	changed.Resource.ID = "changed"
	if _, err := svc.Send(ctx, "account-api", "integration-send", changed); !errors.Is(err, service.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Send() error = %v", err)
	}

	var messages, deliveries, outbox int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_messages WHERE id=$1`, first.MessageID).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries WHERE message_id=$1`, first.MessageID).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM notification_outbox o
		JOIN notification_deliveries d ON d.id=o.delivery_id
		WHERE d.message_id=$1`, first.MessageID).Scan(&outbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if messages != 1 || deliveries != 1 || outbox != 1 {
		t.Fatalf("ledger counts messages=%d deliveries=%d outbox=%d", messages, deliveries, outbox)
	}

	var targetCiphertext, payloadCiphertext []byte
	if err := db.QueryRowContext(ctx, `
		SELECT target_ciphertext, payload_ciphertext
		FROM notification_messages WHERE id=$1`, first.MessageID).Scan(&targetCiphertext, &payloadCiphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(targetCiphertext, []byte("integration@example.com")) ||
		bytes.Contains(payloadCiphertext, []byte("integration")) {
		t.Fatal("ledger contains plaintext recipient or payload")
	}

	secondRequest := request
	secondRequest.Resource.ID = "second"
	if _, err := svc.Send(ctx, "account-api", "integration-limited", secondRequest); !errors.Is(err, service.ErrRateLimited) {
		t.Fatalf("rate-limited Send() error = %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_messages WHERE idempotency_key='integration-limited'`).Scan(&messages); err != nil {
		t.Fatalf("count limited messages: %v", err)
	}
	if messages != 0 {
		t.Fatalf("rate-limited messages = %d, want 0", messages)
	}
}

func testDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if !strings.Contains(strings.ToLower(strings.TrimPrefix(parsed.Path, "/")), "test") {
		t.Fatal("TEST_DATABASE_URL database name must contain test")
	}

	admin, err := database.Open(config.Config{DatabaseURL: rawURL})
	if err != nil {
		t.Fatalf("database.Open(admin): %v", err)
	}
	schema := "notification_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	})

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped, err := database.Open(config.Config{DatabaseURL: parsed.String()})
	if err != nil {
		admin.Close()
		t.Fatalf("database.Open(scoped): %v", err)
	}
	return admin, scoped
}

func testMigrationAdvisoryLock(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("lock connection: %v", err)
	}
	defer connection.Close()

	const lock = "hhc_notification_api_migrations"
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, lock); err != nil {
		t.Fatalf("acquire migration advisory lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, lock)
		}
	}()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- migrations.Run(ctx, db)
	}()
	<-started

	select {
	case err := <-done:
		t.Fatalf("migrations.Run() returned while lock held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, lock); err != nil {
		t.Fatalf("release migration advisory lock: %v", err)
	}
	locked = false

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migrations.Run() after lock release error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("migrations.Run() remained blocked after lock release")
	}
}

func testSkipLocked(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	messageID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO notification_messages (
			id, caller_app_id, idempotency_key, request_hash, template_id, template_version,
			channel, target_type, target_hash, target_ciphertext, payload_ciphertext,
			resource_type, resource_id, status
		) VALUES ($1::uuid, 'integration', $2::text, 'hash', 'test', 1, 'email', 'email', 'target', '\x01', '\x02', 'test', $2::text, 'queued')`,
		messageID, messageID,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO notification_deliveries (id, message_id, channel, provider, status)
			VALUES ($1, $2, 'email', 'test', 'queued')`,
			uuid.NewString(), messageID,
		); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
	}

	first, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	defer first.Rollback()
	firstID := lockedDelivery(t, ctx, first)

	second, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("second transaction: %v", err)
	}
	defer second.Rollback()
	secondID := lockedDelivery(t, ctx, second)
	if firstID == secondID {
		t.Fatalf("FOR UPDATE SKIP LOCKED returned locked delivery %s", firstID)
	}
}

func lockedDelivery(t *testing.T, ctx context.Context, tx *sql.Tx) string {
	t.Helper()
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM notification_deliveries
		WHERE status='queued' AND next_attempt_at <= CURRENT_TIMESTAMP
		ORDER BY created_at, id
		FOR UPDATE SKIP LOCKED LIMIT 1`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("select queued delivery: %v", err)
	}
	return id
}
