//go:build integration

package retention

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresRetentionTombstonesThenDeletesWithoutLosingReceiptMetadata(t *testing.T) {
	db := retentionTestDatabase(t)
	resetRetentionTables(t, db)
	recentID := insertTerminalMessage(t, db, 8*24*time.Hour, "provider-receipt")
	expiredID := insertTerminalMessage(t, db, 731*24*time.Hour, "old-receipt")
	if _, err := db.Exec(`
		INSERT INTO notification_rate_limits (bucket_key,count,expires_at)
		VALUES ('expired',1,clock_timestamp()-interval '1 second'),
		       ('active',1,clock_timestamp()+interval '1 day')`,
	); err != nil {
		t.Fatalf("insert rate buckets: %v", err)
	}

	result, err := New(db).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Tombstoned != 1 || result.MessagesDeleted != 1 || result.RateBucketsDeleted != 1 {
		t.Fatalf("RunOnce() = %#v", result)
	}

	var targetLength, payloadLength int
	var receipt, status string
	var attempts int
	if err := db.QueryRow(`
		SELECT octet_length(message.target_ciphertext),
		       octet_length(message.payload_ciphertext),
		       delivery.provider_message_id,
		       delivery.status,
		       delivery.attempt_count
		FROM notification_messages message
		JOIN notification_deliveries delivery ON delivery.message_id=message.id
		WHERE message.id=$1`,
		recentID,
	).Scan(&targetLength, &payloadLength, &receipt, &status, &attempts); err != nil {
		t.Fatalf("read retained metadata: %v", err)
	}
	if targetLength != 0 || payloadLength != 0 ||
		receipt != "provider-receipt" || status != "sent" || attempts != 1 {
		t.Fatalf(
			"retained target=%d payload=%d receipt=%q status=%q attempts=%d",
			targetLength,
			payloadLength,
			receipt,
			status,
			attempts,
		)
	}

	var oldCount, activeBuckets int
	if err := db.QueryRow(`SELECT count(*) FROM notification_messages WHERE id=$1`, expiredID).Scan(&oldCount); err != nil {
		t.Fatalf("count old message: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notification_rate_limits WHERE bucket_key='active'`).Scan(&activeBuckets); err != nil {
		t.Fatalf("count active rate bucket: %v", err)
	}
	if oldCount != 0 || activeBuckets != 1 {
		t.Fatalf("old messages=%d active buckets=%d", oldCount, activeBuckets)
	}
}

func TestPostgresRetentionIsSafeAcrossReplicas(t *testing.T) {
	db := retentionTestDatabase(t)
	resetRetentionTables(t, db)
	for range 20 {
		insertTerminalMessage(t, db, 8*24*time.Hour, "")
	}
	first := New(db)
	second := New(db)
	first.batchSize = 5
	second.batchSize = 5

	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, instance := range []*Worker{first, second} {
		wait.Add(1)
		go func(instance *Worker) {
			defer wait.Done()
			for {
				result, err := instance.RunOnce(context.Background())
				if err != nil {
					errors <- err
					return
				}
				if result.Tombstoned == 0 {
					return
				}
			}
		}(instance)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent RunOnce() error = %v", err)
	}

	var remaining int
	if err := db.QueryRow(`
		SELECT count(*)
		FROM notification_messages
		WHERE payload_purged_at IS NULL`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count unpurged messages: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("unpurged messages = %d, want 0", remaining)
	}
}

func retentionTestDatabase(t *testing.T) *sql.DB {
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
	schema := "retention_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create retention schema: %v", err)
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

func resetRetentionTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		TRUNCATE notification_outbox, notification_deliveries,
		         notification_messages, notification_rate_limits CASCADE`,
	); err != nil {
		t.Fatalf("reset retention tables: %v", err)
	}
}

func insertTerminalMessage(
	t *testing.T,
	db *sql.DB,
	age time.Duration,
	receipt string,
) string {
	t.Helper()
	messageID := uuid.NewString()
	deliveryID := uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin fixture: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO notification_messages (
			id,caller_app_id,idempotency_key,request_hash,template_id,template_version,
			channel,target_type,target_hash,target_ciphertext,payload_ciphertext,
			resource_type,resource_id,status,terminal_at,updated_at
		) VALUES ($1,'account-api',$2,'hash','account.verify-email',1,'email',
		          'email','target-hash',decode('01','hex'),decode('02','hex'),
		          'account','user-1','sent',
		          clock_timestamp()-($3::double precision*interval '1 second'),
		          clock_timestamp()-($3::double precision*interval '1 second'))`,
		messageID, uuid.NewString(), age.Seconds(),
	); err != nil {
		t.Fatalf("insert terminal message: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO notification_deliveries (
			id,message_id,channel,provider,status,attempt_count,sent_at,
			provider_message_id,updated_at
		) VALUES ($1,$2,'email','smtp','sent',1,
		          clock_timestamp()-($4::double precision*interval '1 second'),
		          NULLIF($3,''),
		          clock_timestamp()-($4::double precision*interval '1 second'))`,
		deliveryID, messageID, receipt, age.Seconds(),
	); err != nil {
		t.Fatalf("insert terminal delivery: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return messageID
}
