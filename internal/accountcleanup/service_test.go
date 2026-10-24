package accountcleanup

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/sesfeedback"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"time"
)

func TestAccountCleanupTombstonesWithoutDSRRequest(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "member@example.test"
	insertMessage(t, db, "v1", crypto.Hash(key, []byte(email)), "email", "sent", "")
	service := New(db, map[string][]byte{"v1": key})
	request := AccountCleanupRequest{UserID: uuid.NewString(), Email: email, IdempotencyKey: "delete-1"}

	result, err := service.CleanupAccount(context.Background(), request)
	if err != nil || result.Status != "completed" || result.RecordCount != 1 {
		t.Fatalf("cleanup = %#v, error=%v", result, err)
	}
	repeated, err := service.CleanupAccount(context.Background(), request)
	if err != nil || repeated.RecordCount != result.RecordCount || repeated.Status != result.Status || repeated.RemainingCount != result.RemainingCount {
		t.Fatalf("repeated cleanup = %#v, error=%v", repeated, err)
	}
	request.UserID = uuid.NewString()
	if _, err := service.CleanupAccount(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("idempotency collision error=%v", err)
	}
}

func TestAccountCleanupUsesSubjectAttributionAcrossKeysAndChannels(t *testing.T) {
	db := testDatabase(t)
	keys := map[string][]byte{
		"v1": []byte("11111111111111111111111111111111"),
		"v2": []byte("22222222222222222222222222222222"),
	}
	userID := uuid.NewString()
	currentEmail := "current@example.test"
	legacyCurrent := insertMessage(t, db, "v1", crypto.Hash(keys["v1"], []byte(currentEmail)), "email", "sent", "")
	priorEmail := insertMessage(t, db, "v1", crypto.Hash(keys["v1"], []byte("prior@example.test")), "email", "sent", "")
	webPush := insertMessage(t, db, "v2", crypto.Hash(keys["v2"], []byte("push-target")), "web_push", "sent", "")
	unrelated := insertMessage(t, db, "v2", crypto.Hash(keys["v2"], []byte("other@example.test")), "email", "sent", "")
	setSubjectAttribution(t, db, priorEmail, "v1", crypto.Hash(keys["v1"], []byte("notification-account-subject:"+userID)))
	setSubjectAttribution(t, db, webPush, "v2", crypto.Hash(keys["v2"], []byte("notification-account-subject:"+userID)))
	setSubjectAttribution(t, db, unrelated, "v2", crypto.Hash(keys["v2"], []byte("notification-account-subject:"+uuid.NewString())))

	service := New(db, keys)
	request := AccountCleanupRequest{UserID: userID, Email: currentEmail, IdempotencyKey: "delete-subject-1"}
	result, err := service.CleanupAccount(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.RecordCount != 3 || result.RemainingCount != 0 {
		t.Fatalf("cleanup=%#v", result)
	}
	for _, id := range []string{legacyCurrent, priorEmail, webPush} {
		var target, payload []byte
		var subjectHash, subjectKey sql.NullString
		if err := db.QueryRow(`SELECT target_ciphertext,payload_ciphertext,subject_hash,subject_hash_key_id FROM notification_messages WHERE id=$1`, id).Scan(&target, &payload, &subjectHash, &subjectKey); err != nil {
			t.Fatal(err)
		}
		if len(target) != 0 || len(payload) != 0 || subjectHash.Valid || subjectKey.Valid {
			t.Fatalf("message %s was not fully tombstoned", id)
		}
	}
	var resourceID, idempotencyKey, requestHash, messageStatus, deliveryStatus string
	if err := db.QueryRow(`SELECT resource_id,idempotency_key,request_hash,status FROM notification_messages WHERE id=$1`, priorEmail).Scan(&resourceID, &idempotencyKey, &requestHash, &messageStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM notification_deliveries WHERE message_id=$1`, priorEmail).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if resourceID != "" || !strings.HasPrefix(idempotencyKey, "erased:") || requestHash != strings.Repeat("0", 64) || messageStatus != "sent" || deliveryStatus != "sent" {
		t.Fatalf("retained shell resource=%q key=%q hash=%q message=%q delivery=%q", resourceID, idempotencyKey, requestHash, messageStatus, deliveryStatus)
	}
	var unrelatedTarget []byte
	if err := db.QueryRow(`SELECT target_ciphertext FROM notification_messages WHERE id=$1`, unrelated).Scan(&unrelatedTarget); err != nil {
		t.Fatal(err)
	}
	if len(unrelatedTarget) == 0 {
		t.Fatal("cleanup tombstoned unrelated subject")
	}
	replayed, err := service.CleanupAccount(context.Background(), request)
	if err != nil || replayed.Status != "completed" || replayed.RecordCount != result.RecordCount || replayed.RemainingCount != 0 {
		t.Fatalf("replay=%#v err=%v", replayed, err)
	}
}

func setSubjectAttribution(t *testing.T, db *sql.DB, messageID, keyID, hash string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE notification_messages SET subject_hash_key_id=$2,subject_hash=$3 WHERE id=$1`, messageID, keyID, hash); err != nil {
		t.Fatal(err)
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

func TestEraseDoesNotUseEmailAgainstExplicitOtherSubject(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "same-address@example.test"
	id := insertMessage(t, db, "v1", crypto.Hash(key, []byte(email)), "email", "sent", "")
	setSubjectAttribution(t, db, id, "v1", crypto.Hash(key, []byte("notification-account-subject:"+uuid.NewString())))
	result, err := New(db, map[string][]byte{"v1": key}).CleanupAccount(context.Background(), AccountCleanupRequest{UserID: uuid.NewString(), Email: email, IdempotencyKey: "explicit-subject-boundary"})
	if err != nil || result.RecordCount != 0 || result.RemainingCount != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	var target []byte
	if err := db.QueryRow(`SELECT target_ciphertext FROM notification_messages WHERE id=$1`, id).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if len(target) == 0 {
		t.Fatal("erased another subject's explicitly bound data")
	}
}

func TestAccountErasureClearsTechnicalBlocksAndLateFeedbackDoesNotRestoreThem(t *testing.T) {
	db := testDatabase(t)
	key := []byte("11111111111111111111111111111111")
	email := "member@example.test"
	userID := uuid.NewString()
	priorHash := crypto.Hash(key, []byte("prior@example.test"))
	prior := insertMessage(t, db, "v1", priorHash, "email", "sent", "")
	setSubjectAttribution(t, db, prior, "v1", crypto.Hash(key, []byte("notification-account-subject:"+userID)))
	if _, err := db.Exec(`INSERT INTO notification_email_suppressions(hash_key_id,target_hash,reason) VALUES('v1',$1,'hard_bounce')`, priorHash); err != nil {
		t.Fatal(err)
	}
	hash := crypto.Hash(key, []byte(email))
	id := insertMessage(t, db, "v1", hash, "email", "sent", "")
	var delivery string
	if err := db.QueryRow(`SELECT id::text FROM notification_deliveries WHERE message_id=$1`, id).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE notification_messages SET target_hashes=jsonb_build_object('v1',$2::text) WHERE id=$1`, id, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,provider,provider_message_id) VALUES($1,1,false,'accepted','ses','ses-erasure')`, delivery); err != nil {
		t.Fatal(err)
	}
	feedback := sesfeedback.Store{DB: db}
	e := sesfeedback.Event{SNSMessageID: uuid.NewString(), MessageID: "ses-erasure", DeliveryID: delivery, Kind: "complaint", At: time.Now().UTC()}
	if err := feedback.Handle(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	result, err := New(db, map[string][]byte{"v1": key}).CleanupAccount(context.Background(), AccountCleanupRequest{UserID: userID, Email: email, IdempotencyKey: "erase-feedback"})
	if err != nil || result.Status != "completed" || result.RecordCount != 4 {
		t.Fatalf("cleanup=%#v err=%v", result, err)
	}
	e.SNSMessageID = uuid.NewString()
	if err := feedback.Handle(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM notification_email_suppressions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("late feedback restored erased block: %d %v", n, err)
	}
	var aliases string
	if err := db.QueryRow(`SELECT target_hashes::text FROM notification_messages WHERE id=$1`, id).Scan(&aliases); err != nil || aliases != "{}" {
		t.Fatalf("aliases retained: %s %v", aliases, err)
	}
}
