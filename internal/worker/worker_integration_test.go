//go:build integration

package worker

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/dsr"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresLeaseExcludesConcurrentWorkersAndRecoversAfterExpiry(t *testing.T) {
	db := workerTestDatabase(t)
	resetWorkerTables(t, db)
	key := bytes.Repeat([]byte{1}, 32)
	deliveryID := insertWorkerDelivery(t, db, key, statusQueued, 0, nil)
	if _, err := db.Exec(`
		INSERT INTO notification_outbox (id,delivery_id,status)
		VALUES ($1,$2,'pending')`,
		uuid.NewString(),
		deliveryID,
	); err != nil {
		t.Fatalf("insert retry outbox: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	firstProvider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		calls.Add(1)
		close(started)
		<-release
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	secondProvider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		calls.Add(1)
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	firstMessage := &fakeMessage{id: deliveryID}
	secondMessage := &fakeMessage{id: deliveryID}
	firstDone := make(chan error, 1)

	go func() {
		firstDone <- New(db, firstProvider, key).Process(context.Background(), firstMessage)
	}()
	<-started
	var messageStatus string
	if err := db.QueryRow(`
		SELECT message.status
		FROM notification_messages message
		JOIN notification_deliveries delivery ON delivery.message_id=message.id
		WHERE delivery.id=$1`,
		deliveryID,
	).Scan(&messageStatus); err != nil {
		t.Fatalf("read in-flight message status: %v", err)
	}
	if messageStatus != statusSending {
		t.Fatalf("in-flight message status = %q, want %q", messageStatus, statusSending)
	}
	if _, err := db.Exec(`
		UPDATE notification_messages AS message
		SET expires_at=clock_timestamp()-interval '1 second'
		FROM notification_deliveries AS delivery
		WHERE delivery.id=$1 AND message.id=delivery.message_id`,
		deliveryID,
	); err != nil {
		t.Fatalf("expire in-flight message: %v", err)
	}
	if err := New(db, secondProvider, key).Process(context.Background(), secondMessage); err == nil {
		t.Fatal("second Process() error = nil while delivery fence is held")
	}
	if secondMessage.completed != 0 || secondMessage.deadLettered != 0 {
		t.Fatalf("second settlement complete=%d dead-letter=%d", secondMessage.completed, secondMessage.deadLettered)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}

	expired := time.Now().Add(-time.Minute)
	recoveredID := insertWorkerDelivery(t, db, key, statusSending, 1, &expired)
	recovered := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	if err := New(db, recovered, key).Process(context.Background(), &fakeMessage{id: recoveredID}); err != nil {
		t.Fatalf("expired lease Process() error = %v", err)
	}
	if recovered.calls != 1 {
		t.Fatalf("recovered provider calls = %d, want 1", recovered.calls)
	}
}

func TestPostgresEraseWaitsForClaimedProviderCall(t *testing.T) {
	db := workerTestDatabase(t)
	resetWorkerTables(t, db)
	key := bytes.Repeat([]byte{1}, 32)
	email := "user@example.com"
	deliveryID := insertWorkerDelivery(t, db, key, statusQueued, 0, nil)
	if _, err := db.Exec(`
		UPDATE notification_messages AS message
		SET hash_key_id='v1', target_hash=$2
		FROM notification_deliveries AS delivery
		WHERE delivery.id=$1 AND message.id=delivery.message_id`,
		deliveryID, notificationcrypto.Hash(key, []byte(email)),
	); err != nil {
		t.Fatal(err)
	}

	providerStarted := make(chan struct{})
	allowProviderReturn := make(chan struct{})
	workerDone := make(chan error, 1)
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		close(providerStarted)
		<-allowProviderReturn
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	go func() {
		workerDone <- New(db, provider, key).Process(context.Background(), &fakeMessage{id: deliveryID})
	}()
	<-providerStarted

	eraseDone := make(chan error, 1)
	go func() {
		_, err := dsr.New(db, map[string][]byte{"v1": key}).Apply(context.Background(), dsr.ActionRequest{
			RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: email, Action: "erase", IdempotencyKey: "erase-race",
		})
		eraseDone <- err
	}()
	waitForAdvisoryWaiters(t, db, 1)
	select {
	case err := <-eraseDone:
		t.Fatalf("erase returned before provider call completed: %v", err)
	default:
	}

	close(allowProviderReturn)
	if err := <-workerDone; err != nil {
		t.Fatalf("worker Process() error = %v", err)
	}
	if err := <-eraseDone; err != nil {
		t.Fatalf("erase error = %v", err)
	}
	var targetLength, payloadLength int
	if err := db.QueryRow(`
		SELECT octet_length(message.target_ciphertext), octet_length(message.payload_ciphertext)
		FROM notification_messages AS message
		JOIN notification_deliveries AS delivery ON delivery.message_id=message.id
		WHERE delivery.id=$1`, deliveryID,
	).Scan(&targetLength, &payloadLength); err != nil {
		t.Fatal(err)
	}
	if targetLength != 0 || payloadLength != 0 {
		t.Fatalf("erased target=%d payload=%d", targetLength, payloadLength)
	}
}

func TestPostgresEraseRollbackLeavesQueuedDeliveryRetryable(t *testing.T) {
	db := workerTestDatabase(t)
	resetWorkerTables(t, db)
	key := bytes.Repeat([]byte{1}, 32)
	email := "user@example.com"
	deliveryID := insertWorkerDelivery(t, db, key, statusQueued, 0, nil)
	if _, err := db.Exec(`
		UPDATE notification_messages AS message
		SET hash_key_id='v1', target_hash=$2
		FROM notification_deliveries AS delivery
		WHERE delivery.id=$1 AND message.id=delivery.message_id`,
		deliveryID, notificationcrypto.Hash(key, []byte(email)),
	); err != nil {
		t.Fatal(err)
	}

	blocker, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	lockKey := "notification-dsr-delivery:" + deliveryID
	if _, err := blocker.ExecContext(context.Background(), `SELECT pg_advisory_lock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = blocker.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
		}
	}()

	eraseCtx, cancelErase := context.WithCancel(context.Background())
	defer cancelErase()
	eraseDone := make(chan error, 1)
	go func() {
		_, err := dsr.New(db, map[string][]byte{"v1": key}).Apply(eraseCtx, dsr.ActionRequest{
			RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: email, Action: "erase", IdempotencyKey: "erase-rollback",
		})
		eraseDone <- err
	}()
	waitForAdvisoryWaiters(t, db, 1)

	message := &fakeMessage{id: deliveryID}
	err = New(db, &integrationProvider{}, key).Process(context.Background(), message)
	if err == nil {
		t.Fatal("Process() error = nil while erase holds the delivery fence")
	}
	if message.completed != 0 || message.deadLettered != 0 {
		t.Fatalf("settlement complete=%d dead-letter=%d", message.completed, message.deadLettered)
	}

	cancelErase()
	if err := <-eraseDone; err == nil {
		t.Fatal("erase error = nil after cancellation")
	}
	if _, err := blocker.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatal(err)
	}
	locked = false

	var status string
	if err := db.QueryRow(`SELECT status FROM notification_deliveries WHERE id=$1`, deliveryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != statusQueued {
		t.Fatalf("delivery status = %q, want %q", status, statusQueued)
	}
}

func TestPostgresDeliveryTransitionsFenceExpiredClaim(t *testing.T) {
	db := workerTestDatabase(t)
	resetWorkerTables(t, db)
	key := bytes.Repeat([]byte{1}, 32)
	deliveryID := insertWorkerDelivery(t, db, key, statusQueued, 0, nil)
	repository := postgresStore{db: db}

	first, err := repository.claim(context.Background(), deliveryID, leaseDuration)
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %#v, error = %v", first, err)
	}
	if _, err := db.Exec(`
		UPDATE notification_deliveries
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`,
		deliveryID,
	); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	second, err := repository.claim(context.Background(), deliveryID, leaseDuration)
	if err != nil || !second.Claimed {
		t.Fatalf("second claim = %#v, error = %v", second, err)
	}

	receipt := providers.ProviderReceipt{Provider: "smtp", ProviderMessageID: "new"}
	if err := repository.markSent(context.Background(), first.Claim, receipt); err != ErrLeaseLost {
		t.Fatalf("stale markSent() error = %v, want ErrLeaseLost", err)
	}
	if err := repository.markSent(context.Background(), second.Claim, receipt); err != nil {
		t.Fatalf("current markSent() error = %v", err)
	}
}

func TestPostgresExpiresNotificationBeforeProviderSend(t *testing.T) {
	db := workerTestDatabase(t)
	resetWorkerTables(t, db)
	key := bytes.Repeat([]byte{1}, 32)
	deliveryID := insertWorkerDelivery(t, db, key, statusQueued, 0, nil)
	if _, err := db.Exec(`
		UPDATE notification_messages AS message
		SET expires_at=clock_timestamp()-interval '1 second'
		FROM notification_deliveries AS delivery
		WHERE delivery.id=$1 AND message.id=delivery.message_id`,
		deliveryID,
	); err != nil {
		t.Fatalf("expire message: %v", err)
	}
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		t.Fatal("provider called for expired notification")
		return providers.ProviderReceipt{}, nil
	}}
	message := &fakeMessage{id: deliveryID}

	if err := New(db, provider, key).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	var deliveryStatus, messageStatus, errorCode string
	var outboxCount int
	if err := db.QueryRow(`
		SELECT delivery.status, message.status, delivery.last_error_code,
		       (SELECT count(*) FROM notification_outbox
		        WHERE delivery_id=delivery.id AND status<>'published')
		FROM notification_deliveries AS delivery
		JOIN notification_messages AS message ON message.id=delivery.message_id
		WHERE delivery.id=$1`,
		deliveryID,
	).Scan(&deliveryStatus, &messageStatus, &errorCode, &outboxCount); err != nil {
		t.Fatalf("read expired state: %v", err)
	}
	if provider.calls != 0 || message.deadLettered != 1 ||
		deliveryStatus != statusDeadLettered || messageStatus != statusDeadLettered ||
		errorCode != "expired" || outboxCount != 0 {
		t.Fatalf(
			"provider=%d broker=%d delivery=%q message=%q error=%q outbox=%d",
			provider.calls,
			message.deadLettered,
			deliveryStatus,
			messageStatus,
			errorCode,
			outboxCount,
		)
	}
}

func TestPostgresPersistsRetryAndTerminalTransitions(t *testing.T) {
	db := workerTestDatabase(t)
	key := bytes.Repeat([]byte{1}, 32)
	tests := []struct {
		name             string
		initialAttempts  int
		providerError    error
		wantStatus       string
		wantOutbox       int
		wantComplete     int
		wantDeadLettered int
	}{
		{
			name:          "temporary retry",
			providerError: &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "send"},
			wantStatus:    statusQueued,
			wantOutbox:    1,
			wantComplete:  1,
		},
		{
			name:             "permanent failure",
			providerError:    &providers.ProviderError{Kind: providers.ErrorPermanent, Operation: "send"},
			wantStatus:       statusFailed,
			wantDeadLettered: 1,
		},
		{
			name:             "retry exhaustion",
			initialAttempts:  maxAttempts - 1,
			providerError:    &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "send"},
			wantStatus:       statusDeadLettered,
			wantDeadLettered: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetWorkerTables(t, db)
			deliveryID := insertWorkerDelivery(
				t,
				db,
				key,
				statusQueued,
				test.initialAttempts,
				nil,
			)
			message := &fakeMessage{id: deliveryID}
			provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
				return providers.ProviderReceipt{}, test.providerError
			}}

			if err := New(db, provider, key).Process(context.Background(), message); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			var deliveryStatus, messageStatus string
			var outboxCount int
			if err := db.QueryRow(`
				SELECT delivery.status, message.status,
				       (SELECT count(*) FROM notification_outbox
				        WHERE delivery_id=delivery.id)
				FROM notification_deliveries delivery
				JOIN notification_messages message ON message.id=delivery.message_id
				WHERE delivery.id=$1`,
				deliveryID,
			).Scan(&deliveryStatus, &messageStatus, &outboxCount); err != nil {
				t.Fatalf("read transition: %v", err)
			}
			if deliveryStatus != test.wantStatus ||
				messageStatus != test.wantStatus ||
				outboxCount != test.wantOutbox {
				t.Fatalf(
					"delivery=%q message=%q outbox=%d, want %q/%q/%d",
					deliveryStatus,
					messageStatus,
					outboxCount,
					test.wantStatus,
					test.wantStatus,
					test.wantOutbox,
				)
			}
			if message.completed != test.wantComplete ||
				message.deadLettered != test.wantDeadLettered {
				t.Fatalf(
					"complete=%d dead-letter=%d, want %d/%d",
					message.completed,
					message.deadLettered,
					test.wantComplete,
					test.wantDeadLettered,
				)
			}
		})
	}
}

func workerTestDatabase(t *testing.T) *sql.DB {
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
	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "worker_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create worker schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return db
}

func resetWorkerTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		TRUNCATE notification_outbox, notification_deliveries,
		         notification_messages, notification_rate_limits CASCADE`,
	); err != nil {
		t.Fatalf("reset worker tables: %v", err)
	}
}

func waitForAdvisoryWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

func insertWorkerDelivery(
	t *testing.T,
	db *sql.DB,
	key []byte,
	status string,
	attempt int,
	leaseExpiresAt *time.Time,
) string {
	t.Helper()
	messageID := uuid.NewString()
	deliveryID := uuid.NewString()
	target, err := notificationcrypto.Encrypt(
		key,
		[]byte(messageID+":target"),
		[]byte("user@example.com"),
	)
	if err != nil {
		t.Fatalf("encrypt target: %v", err)
	}
	payload, err := notificationcrypto.Encrypt(
		key,
		[]byte(messageID+":payload"),
		[]byte(`{"locale":"en","fields":{"verifyUrl":"https://account.alive.org.tw/verify-email?token=opaque"}}`),
	)
	if err != nil {
		t.Fatalf("encrypt payload: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin fixture: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO notification_messages (
			id,caller_app_id,idempotency_key,request_hash,template_id,template_version,
			channel,target_type,target_hash,target_ciphertext,payload_ciphertext,
			resource_type,resource_id,status
		) VALUES ($1,'account-api',$2,'hash','account.verify-email',1,'email',
		          'email','target-hash',$3,$4,'account','user-1',$5)`,
		messageID, uuid.NewString(), target, payload, status,
	); err != nil {
		t.Fatalf("insert message fixture: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO notification_deliveries (
			id,message_id,channel,provider,status,attempt_count,lease_expires_at
		) VALUES ($1,$2,'email','smtp',$3,$4,$5)`,
		deliveryID, messageID, status, attempt, leaseExpiresAt,
	); err != nil {
		t.Fatalf("insert delivery fixture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return deliveryID
}

type integrationProvider struct {
	mu    sync.Mutex
	calls int
	send  func(context.Context) (providers.ProviderReceipt, error)
}

func (p *integrationProvider) Send(
	ctx context.Context,
	_ providers.DeliveryPayload,
) (providers.ProviderReceipt, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.send(ctx)
}
