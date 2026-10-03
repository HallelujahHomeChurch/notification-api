//go:build integration

package nativepush

import (
	"bytes"
	"database/sql"
	"encoding/json"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestNativeReceiptsRetryCallbackAndDoNotResendPush(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	u, e := url.Parse(raw)
	if e != nil || !strings.Contains(u.Path, "test") {
		t.Fatal("a task test database is required")
	}
	admin, e := sql.Open("pgx", raw)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "native_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = admin.Exec(`CREATE SCHEMA ` + schema); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, e := sql.Open("pgx", u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrations.Run(t.Context(), db); e != nil {
		t.Fatal(e)
	}
	id, delivery, job := uuid.NewString(), uuid.NewString(), uuid.NewString()
	key := bytes.Repeat([]byte{1}, 32)
	body, _ := json.Marshal(map[string]any{"fields": map[string]string{"deliveryId": job, "assignmentId": uuid.NewString()}})
	encrypted, e := notificationcrypto.Encrypt(key, []byte(id+":payload"), body)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO notification_messages(id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,status) VALUES($1,'operations-api','native-test','hash','operations.native-push',1,'native_push','native_push','hash','encrypted',$2,'service_assignment','assignment','sent')`, id, encrypted)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO notification_deliveries(id,message_id,channel,provider,status,sent_at,provider_message_id) VALUES($1,$2,'native_push','expo','sent',now()-interval '16 minutes','ticket')`, delivery, id)
	if e != nil {
		t.Fatal(e)
	}
	failed := true
	callbacks := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getReceipts":
			w.Write([]byte(`{"data":{"ticket":{"status":"error","details":{"error":"DeviceNotRegistered"}}}}`))
		case "/priv/service-push/result":
			callbacks++
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			if v["deliveryId"] != job || v["result"] != "invalid_endpoint" {
				t.Error(v)
			}
			if failed {
				w.WriteHeader(503)
			} else {
				w.Write([]byte(`{"ok":true}`))
			}
		default:
			t.Error("unexpected provider send", r.URL.Path)
		}
	}))
	defer s.Close()
	p := New(s.URL, "")
	p.ExpoURL = s.URL
	keys := map[string][]byte{"legacy-v1": key}
	if e = p.PollReceipts(t.Context(), db, keys); e == nil {
		t.Fatal("callback failure lost")
	}
	failed = false
	if e = p.PollReceipts(t.Context(), db, keys); e != nil {
		t.Fatal(e)
	}
	if e = p.PollReceipts(t.Context(), db, keys); e != nil {
		t.Fatal(e)
	}
	if callbacks != 2 {
		t.Fatalf("callbacks=%d", callbacks)
	}
	var reason string
	if e = db.QueryRow(`SELECT last_error_code FROM notification_deliveries WHERE id=$1`, delivery).Scan(&reason); e != nil || reason != "native_invalid_endpoint" {
		t.Fatalf("reason=%s err=%v", reason, e)
	}
}
