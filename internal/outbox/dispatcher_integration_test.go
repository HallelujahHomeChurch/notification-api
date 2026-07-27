//go:build integration

package outbox

import (
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
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
)

func TestPostgresClaimsPendingOutboxOnceAcrossReplicas(t *testing.T) {
	db := integrationDatabase(t)
	insertOutboxFixture(t, db, "pending", nil)
	first := postgresStore{db: db}
	second := postgresStore{db: db}

	start := make(chan struct{})
	results := make(chan bool, 2)
	var wait sync.WaitGroup
	for _, store := range []postgresStore{first, second} {
		wait.Add(1)
		go func(store postgresStore) {
			defer wait.Done()
			<-start
			_, ok, err := store.claimNext(context.Background(), leaseDuration)
			if err != nil {
				t.Errorf("claimNext() error = %v", err)
				return
			}
			results <- ok
		}(store)
	}
	close(start)
	wait.Wait()
	close(results)

	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("successful claims = %d, want 1", claimed)
	}
}

func TestPostgresRecoversExpiredLeaseUsingDatabaseClock(t *testing.T) {
	db := integrationDatabase(t)
	expired := time.Now().Add(-time.Hour)
	outboxID := insertOutboxFixture(t, db, "publishing", &expired)

	claimed, ok, err := (postgresStore{db: db}).claimNext(context.Background(), leaseDuration)
	if err != nil {
		t.Fatalf("claimNext() error = %v", err)
	}
	if !ok || claimed.OutboxID != outboxID || claimed.Attempt != 2 {
		t.Fatalf("claim = %#v ok=%v", claimed, ok)
	}

	var remainingSeconds float64
	if err := db.QueryRow(`
		SELECT EXTRACT(EPOCH FROM (lease_expires_at-clock_timestamp()))
		FROM notification_outbox WHERE id=$1`, outboxID).Scan(&remainingSeconds); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if remainingSeconds < 55 || remainingSeconds > 60 {
		t.Fatalf("lease remaining = %.3fs, want database-clock lease near 60s", remainingSeconds)
	}
}

func TestPostgresFencesStaleLeaseGenerationTransitions(t *testing.T) {
	db := integrationDatabase(t)
	outboxID := insertOutboxFixture(t, db, "pending", nil)
	store := postgresStore{db: db}

	generation1, ok, err := store.claimNext(context.Background(), leaseDuration)
	if err != nil || !ok {
		t.Fatalf("claim generation 1 = %#v ok=%v error=%v", generation1, ok, err)
	}
	if _, err := db.Exec(`
		UPDATE notification_outbox
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, outboxID); err != nil {
		t.Fatalf("expire generation 1: %v", err)
	}
	generation2, ok, err := store.claimNext(context.Background(), leaseDuration)
	if err != nil || !ok {
		t.Fatalf("claim generation 2 = %#v ok=%v error=%v", generation2, ok, err)
	}
	if generation1.Attempt != 1 || generation2.Attempt != 2 {
		t.Fatalf("attempt generations = %d, %d", generation1.Attempt, generation2.Attempt)
	}

	if err := store.markPublished(context.Background(), generation1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale markPublished() error = %v, want ErrLeaseLost", err)
	}
	if err := store.markRetry(context.Background(), generation1, time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale markRetry() error = %v, want ErrLeaseLost", err)
	}
	if err := store.markPublished(context.Background(), generation2); err != nil {
		t.Fatalf("generation 2 markPublished() error = %v", err)
	}

	var status string
	var attempt int
	if err := db.QueryRow(`
		SELECT status, attempt_count
		FROM notification_outbox WHERE id=$1`, outboxID).Scan(&status, &attempt); err != nil {
		t.Fatalf("read final outbox state: %v", err)
	}
	if status != "published" || attempt != 2 {
		t.Fatalf("final outbox status=%q attempt=%d", status, attempt)
	}
}

func integrationDatabase(t *testing.T) *sql.DB {
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
		t.Fatalf("open integration database: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "outbox_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := database.Open(config.Config{DatabaseURL: parsed.String()})
	if err != nil {
		t.Fatalf("open scoped integration database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return db
}

func insertOutboxFixture(t *testing.T, db *sql.DB, status string, leaseExpiresAt *time.Time) string {
	t.Helper()
	messageID := uuid.NewString()
	deliveryID := uuid.NewString()
	outboxID := uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin outbox fixture: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO notification_messages (
			id, caller_app_id, idempotency_key, request_hash, template_id, template_version,
			channel, target_type, target_hash, target_ciphertext, payload_ciphertext,
			resource_type, resource_id, status
		) VALUES ($1,$2,$2,'hash','test',1,'email','email','target','\x01','\x02','test',$2,'queued')`,
		messageID, "fixture-"+messageID,
	); err != nil {
		t.Fatalf("insert message fixture: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO notification_deliveries (id,message_id,channel,provider,status)
		VALUES ($1,$2,'email','test','queued')`,
		deliveryID, messageID,
	); err != nil {
		t.Fatalf("insert delivery fixture: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO notification_outbox (
			id,delivery_id,status,attempt_count,next_attempt_at,lease_expires_at
		) VALUES ($1,$2,$3,CASE WHEN $3='publishing' THEN 1 ELSE 0 END,clock_timestamp(),$4)`,
		outboxID, deliveryID, status, leaseExpiresAt,
	); err != nil {
		t.Fatalf("insert outbox fixture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit outbox fixture: %v", err)
	}
	return outboxID
}
