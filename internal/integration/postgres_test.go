//go:build integration

package integration

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
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

	if _, err := scoped.ExecContext(ctx, `UPDATE schema_migrations SET checksum='changed' WHERE version='sql/001_initial.sql'`); err != nil {
		t.Fatalf("corrupt migration checksum: %v", err)
	}
	if err := migrations.Run(ctx, scoped); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("migrations.Run() error = %v, want checksum mismatch", err)
	}

	testSkipLocked(t, ctx, scoped)
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
