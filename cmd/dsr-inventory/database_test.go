package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func inventoryTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for disposable PostgreSQL regression")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		t.Fatal("local disposable PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	name := "inventory_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`); admin.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db, name
}

func TestPostgresInventory(t *testing.T) {
	db, name := inventoryTestDB(t)
	ctx := context.Background()
	tx, err := beginSnapshot(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var mode, isolation, statement, lock, idle, zone string
	if err := tx.QueryRow(`SELECT current_setting('transaction_read_only'),current_setting('transaction_isolation'),current_setting('statement_timeout'),current_setting('lock_timeout'),current_setting('idle_in_transaction_session_timeout'),current_setting('TimeZone')`).Scan(&mode, &isolation, &statement, &lock, &idle, &zone); err != nil {
		t.Fatal(err)
	}
	if mode != "on" || isolation != "repeatable read" || statement != "15s" || lock != "2s" || idle != "30s" || zone != "UTC" {
		t.Fatalf("snapshot limits: %s %s %s %s %s %s", mode, isolation, statement, lock, idle, zone)
	}
	if _, err := tx.Exec(`DELETE FROM public.notification_messages`); err == nil {
		t.Fatal("snapshot permitted write")
	}
	tx.Rollback()

	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}
	templates := []string{"account.dsr-received", "account.dsr-completed", "account.verify-email", "account.dsr-received", "account.dsr-unknown"}
	for i, id := range ids {
		caller := "account-api"
		if i == 3 {
			caller = "other-api"
		}
		_, err := db.Exec(`INSERT INTO notification_messages (id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,status) VALUES($1::uuid,$2,$1::text,'request-secret',$3,1,'email','email','recipient-secret','recipient-private','payload-private','account','account-id','queued')`, id, caller, templates[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	delivery, outbox := uuid.NewString(), uuid.NewString()
	if _, err := db.Exec(`INSERT INTO notification_deliveries(id,message_id,channel,provider,status,lease_expires_at,last_error_code,provider_message_id) VALUES($1,$2,'email','smtp','sending',now()+interval '1 minute','provider-private','provider-id-private')`, delivery, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notification_outbox(id,delivery_id,status) VALUES($1,$2,'pending')`, outbox, delivery); err != nil {
		t.Fatal(err)
	}
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+delivery); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "notification-dsr-delivery:"+delivery)
	data, err := inventory(ctx, db, name, ids[:1])
	if err != nil {
		t.Fatal(err)
	}
	var result snapshot
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Sections["messages"]) != 1 || len(result.Sections["deliveries"]) != 1 || len(result.Sections["outbox"]) != 1 || len(result.Sections["unknownNotApproved"]) != 3 || !strings.Contains(string(result.Sections["deliveryFences"][0]), `"held":true`) {
		t.Fatalf("wrong selection: %s", data)
	}
	for _, secret := range []string{"recipient-secret", "recipient-private", "payload-private", "provider-private", "provider-id-private", "request-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("private field leaked: %s", secret)
		}
	}
	for _, selected := range [][]string{{uuid.NewString()}, {ids[2]}, {ids[3]}, {ids[4]}, {ids[0], uuid.NewString()}} {
		if data, err := inventory(ctx, db, name, selected); err == nil || data != nil {
			t.Fatal("unapproved linkage emitted inventory")
		}
	}
	if data, err := inventory(ctx, db, "wrong", ids[:1]); err == nil || data != nil {
		t.Fatal("wrong DB accepted")
	}
	if _, err := db.Exec(`CREATE TABLE unexpected_dependency(id uuid REFERENCES notification_messages(id))`); err != nil {
		t.Fatal(err)
	}
	if data, err := inventory(ctx, db, name, ids[:1]); err == nil || data != nil {
		t.Fatal("unknown dependency accepted")
	}
	if _, err := db.Exec(`DROP TABLE unexpected_dependency`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`TRUNCATE notification_messages CASCADE`); err != nil {
		t.Fatal(err)
	}
	// A nonempty Account-linked ID set must never report complete when the DB is empty.
	if data, err := inventory(ctx, db, name, ids[:1]); err == nil || data != nil {
		t.Fatal("empty linkage reported complete")
	}
}

func TestPostgresBounds(t *testing.T) {
	db, _ := inventoryTestDB(t)
	ctx := context.Background()
	for _, q := range []string{`SELECT to_json(g) FROM generate_series(1,10001) g`, `SELECT to_json(repeat('x',4194305))`} {
		tx, err := beginSnapshot(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if err := collect(ctx, tx, []query{{name: "oversized", sql: q}}, map[string][]json.RawMessage{}); err == nil {
			t.Fatal("bound accepted")
		}
		tx.Rollback()
	}
	tx, err := beginSnapshot(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string][]json.RawMessage{}
	if err := collect(ctx, tx, []query{{name: "empty", sql: `SELECT to_json(1) WHERE false`}}, sections); err != nil || len(sections["empty"]) != 0 {
		t.Fatal("complete empty section failed")
	}
	tx.Rollback()
}
