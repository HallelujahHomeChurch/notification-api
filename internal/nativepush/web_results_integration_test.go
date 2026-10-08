//go:build integration

package nativepush

import (
	"bytes"
	"database/sql"
	"encoding/json"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/retention"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestWebResultsRetryOnlyCallback(t *testing.T) {
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

	key := bytes.Repeat([]byte{2}, 32)
	results := map[string]string{}
	calls := 0
	unavailable := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/priv/service-push/result" {
			t.Error("unexpected resend or external receipt lookup")
		}
		if unavailable {
			w.WriteHeader(503)
			return
		}
		var result map[string]string
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			t.Error(err)
		}
		results[result["deliveryId"]] = result["result"]
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	p := &WebProvider{Operations: New(server.URL, "")}
	want := map[string]string{}
	for _, tc := range []struct{ status, code, result string }{{"sent", "", "provider_received"}, {"failed", "invalid_endpoint", "invalid_endpoint"}, {"dead_lettered", "temporary", "provider_failed"}} {
		message, delivery, job := uuid.NewString(), uuid.NewString(), uuid.NewString()
		want[job] = tc.result
		raw, _ := json.Marshal(map[string]any{"fields": map[string]string{"deliveryId": job, "assignmentId": uuid.NewString()}})
		encrypted, err := notificationcrypto.Encrypt(key, []byte(message+":payload"), raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO notification_messages(id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,status) VALUES($1::uuid,'operations-api',$4,'hash','operations.web-push',1,'web_push','web_push','hash','encrypted',$2,'service_assignment','assignment',$3)`, message, encrypted, tc.status, "service:"+job)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO notification_deliveries(id,message_id,channel,provider,status,last_error_code) VALUES($1,$2,'web_push','webpush',$3,$4)`, delivery, message, tc.status, tc.code)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := p.PollResults(t.Context(), db); err == nil {
		t.Fatal("callback outage not retained")
	}
	// Retention must erase payloads without losing durable callback identity.
	if _, err := db.Exec(`UPDATE notification_messages SET terminal_at=now()-interval '8 days'`); err != nil {
		t.Fatal(err)
	}
	retained, err := retention.New(db).RunOnce(t.Context())
	if err != nil || retained.Tombstoned != 3 {
		t.Fatalf("retention: %+v %v", retained, err)
	}
	unavailable = false
	if err := p.PollResults(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	for id, value := range want {
		if results[id] != value {
			t.Fatalf("wrong callback %s", id)
		}
	}
	before := calls
	if err := p.PollResults(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("reported outcomes were replayed")
	}
}
