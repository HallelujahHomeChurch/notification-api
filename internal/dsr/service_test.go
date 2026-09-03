package dsr

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestExportFindsMessagesAcrossRetainedHashKeys(t *testing.T) {
	db := testDatabase(t)
	keys := map[string][]byte{"v1": []byte("11111111111111111111111111111111"), "v2": []byte("22222222222222222222222222222222")}
	email := "member@example.test"
	insertMessage(t, db, "v1", crypto.Hash(keys["v1"], []byte(email)), "email", "sent", "")
	insertMessage(t, db, "v2", crypto.Hash(keys["v2"], []byte(email)), "web_push", "failed", "temporary")

	request := ExportRequest{RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: " MEMBER@EXAMPLE.TEST ", Limit: 1}
	page, err := New(db, keys).Export(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if page.RecordCount != 1 || len(page.Records) != 1 || page.NextCursor == "" {
		t.Fatalf("first export = %#v, want one rotated-key record and cursor", page)
	}
	request.Cursor = page.NextCursor
	page, err = New(db, keys).Export(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if page.RecordCount != 1 || len(page.Records) != 1 || page.NextCursor != "" {
		t.Fatalf("second export = %#v, want remaining rotated-key record", page)
	}
}

func TestExportReturnsMetadataWithoutCiphertextOrProviderIdentifiers(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "member@example.test"
	insertMessage(t, db, "v1", crypto.Hash(key, []byte(email)), "email", "sent", "temporary")

	page, err := New(db, map[string][]byte{"v1": key}).Export(context.Background(), ExportRequest{RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: email})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("records = %#v", page.Records)
	}
	record := page.Records[0]
	if record.TemplateID != "account.verify-email" || record.Channel != "email" || record.Status != "sent" || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.DeliveryStatus != "sent" {
		t.Fatalf("record = %#v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ciphertext", "provider", "endpoint", "temporary"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("record leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestEraseTombstonesAttributedPayloadAndBreaksEmailLookup(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "member@example.test"
	hash := crypto.Hash(key, []byte(email))
	messageID := insertMessage(t, db, "v1", hash, "email", "sent", "")
	service := New(db, map[string][]byte{"v1": key})
	request := ActionRequest{RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: email, Action: "erase", IdempotencyKey: "erase-1"}

	result, err := service.Apply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.RecordCount != 1 {
		t.Fatalf("erase = %#v", result)
	}
	repeated, err := service.Apply(context.Background(), request)
	if err != nil || repeated.Status != "completed" || repeated.Action != "erase" || repeated.Owner != "notification-api" {
		t.Fatalf("repeated erase = %#v, error=%v", repeated, err)
	}
	page, err := service.Export(context.Background(), ExportRequest{RequestID: request.RequestID, UserID: request.UserID, Email: email})
	if err != nil {
		t.Fatal(err)
	}
	if page.RecordCount != 0 {
		t.Fatalf("erased lookup returned %#v", page.Records)
	}
	var target, payload []byte
	var receipt, status string
	if err := db.QueryRow(`SELECT target_ciphertext,payload_ciphertext FROM notification_messages WHERE id=$1`, messageID).Scan(&target, &payload); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT provider_message_id,status FROM notification_deliveries WHERE message_id=$1`, messageID).Scan(&receipt, &status); err != nil {
		t.Fatal(err)
	}
	if len(target) != 0 || len(payload) != 0 || receipt != "receipt-id" || status != "sent" {
		t.Fatalf("tombstone target=%q payload=%q receipt=%q status=%q", target, payload, receipt, status)
	}
}

func TestRestrictProcessingReturnsNotApplicableWithoutMutation(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "member@example.test"
	messageID := insertMessage(t, db, "v1", crypto.Hash(key, []byte(email)), "email", "sent", "")

	result, err := New(db, map[string][]byte{"v1": key}).Apply(context.Background(), ActionRequest{RequestID: uuid.NewString(), UserID: uuid.NewString(), Email: email, Action: "restrict_processing", IdempotencyKey: "restrict-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "not_applicable" || result.RecordCount != 0 || !strings.Contains(strings.Join(result.ReasonCodes, ","), "ordinary_receipt_retention") {
		t.Fatalf("restrict result=%#v", result)
	}
	var target, payload []byte
	if err := db.QueryRow(`SELECT target_ciphertext,payload_ciphertext FROM notification_messages WHERE id=$1`, messageID).Scan(&target, &payload); err != nil {
		t.Fatal(err)
	}
	if len(target) == 0 || len(payload) == 0 {
		t.Fatalf("restrict processing mutated message target=%q payload=%q", target, payload)
	}
}

func testDatabase(t *testing.T) *sql.DB {
	t.Helper()
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(strings.TrimPrefix(parsed.Path, "/")), "test") {
		t.Fatal("TEST_DATABASE_URL database name must contain test")
	}
	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "notification_dsr_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertMessage(t *testing.T, db *sql.DB, hashKeyID, targetHash, channel, status, failureCode string) string {
	t.Helper()
	messageID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO notification_messages (
			id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,hash_key_id,status,terminal_at,created_at,updated_at
		) VALUES ($1,'account-api',$2,'request','account.verify-email',1,$3,$3,$4,decode('0102','hex'),decode('0304','hex'),'account','user-1',$5,$6,clock_timestamp(),clock_timestamp(),clock_timestamp())`,
		messageID, messageID, channel, targetHash, hashKeyID, status,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notification_deliveries (id,message_id,channel,provider,status,provider_message_id,last_error_code) VALUES ($1,$2,$3,'smtp',$4,'receipt-id',NULLIF($5,''))`, uuid.NewString(), messageID, channel, status, failureCode); err != nil {
		t.Fatal(err)
	}
	return messageID
}
