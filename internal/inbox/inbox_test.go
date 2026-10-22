package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/accountcleanup"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/retention"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const actor = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

var keys = map[string][]byte{"test": bytes.Repeat([]byte{7}, 32)}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.Contains(u.Path, "test") {
		t.Fatal("test database required")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "inbox_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close(); admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	if err = migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
func write(series string, at time.Time) Write {
	return Write{RecipientAccountID: actor, Type: "bulletin.published", LogicalKey: "bulletin/issue/" + series, Resource: Resource{Type: "bulletin", ID: "issue", Series: series}, EventVersion: 1, OccurredAt: at}
}

type policy struct {
	denySeries  string
	unavailable bool
}

func (p *policy) Check(_ context.Context, _ string, _ string, refs []Ref) (map[string]Projection, error) {
	if p.unavailable {
		return nil, ErrUnavailable
	}
	out := map[string]Projection{}
	for _, ref := range refs {
		if ref.Resource.Series != p.denySeries {
			out[ref.ID] = Projection{TitleKey: "notification.bulletin", MessageKey: "notification.published", Parameters: map[string]string{"title": ref.Resource.Series}, Action: &Action{Type: "bulletin", Resource: ref.Resource, Locale: "en"}}
		}
	}
	return out, nil
}
func mustAccept(t *testing.T, s *Store, writes ...Write) {
	t.Helper()
	if _, err := s.AcceptBatch(context.Background(), "engagement-api", writes, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
func TestSeriesVisibilityReplayAndReadSnapshot(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	p := &policy{denySeries: "children"}
	s := NewService(store, p, "test", keys)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	mustAccept(t, store, write("general", now), write("children", now))
	summary, err := s.Summary(ctx, actor, "zh-Hant")
	if err != nil {
		t.Fatal(err)
	}
	if summary.UnreadCount != 1 || len(summary.Items) != 1 || summary.Items[0].Resource.Series != "general" || summary.Items[0].Projection.Action.Resource.Series != "general" {
		t.Fatalf("series leaked: %+v", summary.Items)
	}
	if err = s.MarkAllRead(ctx, actor, summary.ReadSnapshotToken); err != nil {
		t.Fatal(err)
	}
	mustAccept(t, store, write("general", now), write("children", now))
	replay := write("general", now.Add(time.Minute))
	replay.EventVersion = 2
	mustAccept(t, store, replay)
	var expiry time.Time
	var sequence int64
	if err = db.QueryRow(`SELECT expires_at,recipient_sequence FROM notification_inbox WHERE logical_notification_key='bulletin/issue/general'`).Scan(&expiry, &sequence); err != nil || !expiry.Equal(now.Add(Retention)) || sequence != 1 {
		t.Fatalf("replay changed expiry/sequence %v %d %v", expiry, sequence, err)
	}
	p.denySeries = ""
	summary, err = s.Summary(ctx, actor, "zh-Hant")
	if err != nil || summary.UnreadCount != 0 || len(summary.Items) != 2 {
		t.Fatalf("replay reset read: count=%d items=%+v %v", summary.UnreadCount, summary.Items, err)
	}
	late := write("general", now)
	late.LogicalKey = "bulletin/other/general"
	late.Resource.ID = "other"
	mustAccept(t, store, late)
	if err = s.MarkAllRead(ctx, actor, summary.ReadSnapshotToken); err != nil {
		t.Fatal(err)
	}
	summary, err = s.Summary(ctx, actor, "zh-Hant")
	if err != nil || summary.UnreadCount != 1 {
		t.Fatalf("old snapshot consumed new event: count=%d %v", summary.UnreadCount, err)
	}
	p.unavailable = true
	if _, err = s.Summary(ctx, actor, "zh-Hant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("owner failure = %v", err)
	}
}
func TestInboxValidationExpiryConflictAndCleanupFence(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()
	now := time.Now().UTC()
	good := write("general", now)
	for _, producer := range []string{"account-api", "hhc-web-api", "api-gateway", "other"} {
		if _, err := store.AcceptBatch(ctx, producer, []Write{good}, now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("producer %s: %v", producer, err)
		}
	}
	invalid := []Write{good}
	invalid[0].Resource.Series = "adult"
	if _, err := store.AcceptBatch(ctx, "engagement-api", invalid, now); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := store.AcceptBatch(ctx, "operations-api", []Write{good}, now); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := store.AcceptBatch(ctx, "engagement-api", make([]Write, 101), now); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	old := write("general", now.Add(-91*24*time.Hour))
	mustAccept(t, store, old)
	var n int
	if err := db.QueryRow("SELECT count(*) FROM notification_inbox").Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired resurrected: %d %v", n, err)
	}
	mustAccept(t, store, good)
	changed := good
	changed.Resource.ID = "different"
	if _, err := store.AcceptBatch(ctx, "engagement-api", []Write{changed}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	cleanup := accountcleanup.New(db, keys)
	if _, err := cleanup.CleanupAccount(ctx, accountcleanup.AccountCleanupRequest{UserID: actor, Email: "test@example.com", IdempotencyKey: "cleanup"}); err != nil {
		t.Fatal(err)
	}
	mustAccept(t, store, good)
	if err := db.QueryRow("SELECT count(*) FROM notification_inbox").Scan(&n); err != nil || n != 0 {
		t.Fatalf("cleanup replay resurrected: %d %v", n, err)
	}
}
func TestSnapshotCannotConsumeUncommittedInsert(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	s := NewService(store, &policy{}, "test", keys)
	ctx := context.Background()
	now := time.Now().UTC()
	mustAccept(t, store, write("general", now))
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var seq int64
	if err = tx.QueryRow("UPDATE inbox_recipient_state SET committed_sequence=committed_sequence+1 WHERE recipient_account_id=$1 RETURNING committed_sequence", actor).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	_, err = tx.Exec(`INSERT INTO notification_inbox(id,recipient_account_id,recipient_sequence,producer,event_type,logical_notification_key,resource,event_version,occurred_at,expires_at) VALUES($1,$2,$3,'engagement-api','bulletin.published','late','{"type":"bulletin","id":"late","series":"general"}',1,$4,$4::timestamptz+interval '90 days')`, id, actor, seq, now)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Page(ctx, actor, "all", "", 20, "zh-Hant")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("uncommitted item visible: %+v", page.Items)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkAllRead(ctx, actor, page.ReadSnapshotToken); err != nil {
		t.Fatal(err)
	}
	var read sql.NullTime
	if err = db.QueryRow("SELECT read_at FROM notification_inbox WHERE id=$1", id).Scan(&read); err != nil || read.Valid {
		t.Fatalf("late commit consumed: %+v %v", read, err)
	}
}
func TestSignedTokensRejectWrongActorPurposeTamperingAndExpiry(t *testing.T) {
	codec := newTokens("test", keys)
	now := time.Now().UTC()
	tok := codec.sign(token{Actor: actor, Purpose: "read", Sequence: 3, IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix()})
	for _, tc := range []struct {
		value, actor, purpose string
		at                    time.Time
	}{{tok, uuid.NewString(), "read", now}, {tok, actor, "cursor", now}, {tok + "a", actor, "read", now}, {tok, actor, "read", now.Add(15 * time.Minute)}} {
		if _, err := codec.verify(tc.value, tc.actor, tc.purpose, tc.at); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid token: %v", err)
		}
	}
	if _, err := codec.verify(tok, actor, "read", now); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedPageCanBeEmptyWhileExactSummaryCountsAboveOneHundred(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()
	now := time.Now().UTC()
	p := &policy{denySeries: "general"}
	svc := NewService(store, p, "test", keys)
	for batch := 0; batch < 7; batch++ {
		items := []Write{}
		for index := 0; index < 100; index++ {
			w := write("general", now.Add(-time.Duration(batch*100+index)*time.Second))
			w.LogicalKey = uuid.NewString()
			w.Resource.ID = w.LogicalKey
			items = append(items, w)
		}
		mustAccept(t, store, items...)
	}
	mustAccept(t, store, write("children", now.Add(-701*time.Second)))
	page, err := svc.Page(ctx, actor, "all", "", 20, "zh-Hant")
	if err != nil || len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("bounded hidden scan items=%d cursorPresent=%t %v", len(page.Items), page.NextCursor != "", err)
	}
	next, err := svc.Page(ctx, actor, "all", page.NextCursor, 20, "zh-Hant")
	if err != nil || len(next.Items) != 1 || next.NextCursor != "" {
		t.Fatalf("last hidden page items=%d cursorPresent=%t %v", len(next.Items), next.NextCursor != "", err)
	}
	if _, err = svc.Page(ctx, uuid.NewString(), "all", page.NextCursor, 20, "zh-Hant"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-actor cursor: %v", err)
	}
	if _, err = svc.Page(ctx, actor, "unread", page.NextCursor, 20, "zh-Hant"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-filter cursor: %v", err)
	}
	if _, err = svc.Page(ctx, actor, "all", page.NextCursor+"bad", 20, "zh-Hant"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered cursor: %v", err)
	}
	if _, err = svc.Page(ctx, actor, "all", page.NextCursor, 20, "en"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-locale cursor: %v", err)
	}
	preview, err := svc.Summary(ctx, actor, "zh-Hant")
	if err != nil || preview.UnreadCount != 1 || len(preview.Items) != 1 || preview.Items[0].Resource.Series != "children" {
		t.Fatalf("preview lost visible item beyond bounded page: count=%d items=%+v %v", preview.UnreadCount, preview.Items, err)
	}
	p.denySeries = ""
	summary, err := svc.Summary(ctx, actor, "zh-Hant")
	if err != nil || summary.UnreadCount != 701 || len(summary.Items) != 5 {
		t.Fatalf("exact count %d items=%d %v", summary.UnreadCount, len(summary.Items), err)
	}
}

type failOnResource struct{ policy }

func (p *failOnResource) Check(ctx context.Context, actor, locale string, refs []Ref) (map[string]Projection, error) {
	for _, ref := range refs {
		if ref.Resource.ID == "fail" {
			return nil, ErrUnavailable
		}
	}
	return p.policy.Check(ctx, actor, locale, refs)
}
func TestUnavailableExactCountDoesNotDisableFullPage(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	svc := NewService(store, &failOnResource{}, "test", keys)
	now := time.Now().UTC()
	ctx := context.Background()
	for batch := 0; batch < 2; batch++ {
		items := []Write{}
		for index := 0; index < 100; index++ {
			w := write("general", now.Add(-time.Duration(batch*100+index)*time.Second))
			w.LogicalKey = uuid.NewString()
			w.Resource.ID = w.LogicalKey
			if batch == 1 && index == 99 {
				w.Resource.ID = "fail"
			}
			items = append(items, w)
		}
		mustAccept(t, store, items...)
	}
	if _, err := svc.Summary(ctx, actor, "zh-Hant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial count returned: %v", err)
	}
	page, err := svc.Page(ctx, actor, "all", "", 20, "zh-Hant")
	if err != nil || len(page.Items) != 20 {
		t.Fatalf("page blocked by count: items=%+v %v", page.Items, err)
	}
	if err := svc.MarkRead(ctx, uuid.NewString(), page.Items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-recipient read: %v", err)
	}
	if err := svc.MarkRead(ctx, actor, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkRead(ctx, actor, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestInboxRetentionDeletesExpiredReferencesAndReplayStaysExpired(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	now := time.Now().UTC()
	w := write("general", now)
	mustAccept(t, store, w)
	// Simulate the original event reaching its fixed 90-day expiry.
	if _, err := db.Exec(`UPDATE notification_inbox SET occurred_at=$1,expires_at=$1::timestamptz+interval '2160 hours'`, now.Add(-91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.New(db).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM notification_inbox").Scan(&n); err != nil || n != 0 {
		t.Fatalf("retained expired inbox %d %v", n, err)
	}
	w.OccurredAt = now.Add(-91 * 24 * time.Hour)
	mustAccept(t, store, w)
	if err := db.QueryRow("SELECT count(*) FROM notification_inbox").Scan(&n); err != nil || n != 0 {
		t.Fatalf("replayed expired inbox %d %v", n, err)
	}
}

func TestProducerWaitsForCleanupCommitAndReturnsCleaned(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "notification-account-subject:"+actor); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("notification-account-cleanup:" + actor))
	if _, err = tx.ExecContext(ctx, `INSERT INTO account_cleanup_operations(idempotency_key,subject_ref) VALUES('cleanup-pending',$1)`, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Acceptance
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := NewStore(db).AcceptBatch(ctx, "engagement-api", []Write{write("general", time.Now().UTC())}, time.Now().UTC())
		done <- outcome{result, err}
	}()
	for {
		var waiting bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case early := <-done:
			t.Fatalf("producer did not wait for cleanup: %+v", early)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || len(result.result.Items) != 1 || result.result.Items[0].Status != "cleaned" {
		t.Fatalf("cleanup outcome %+v", result)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM notification_inbox").Scan(&n); err != nil || n != 0 {
		t.Fatalf("cleanup recreated inbox %d %v", n, err)
	}
}
func TestReplayDoesNotAllocateSequenceAndConcurrentRecipientsDoNotDeadlock(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	a := write("general", now)
	b := a
	b.RecipientAccountID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	done := make(chan error, 2)
	for _, batch := range [][]Write{{a, b}, {b, a}} {
		go func(items []Write) { _, err := store.AcceptBatch(ctx, "engagement-api", items, now); done <- err }(batch)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var n, seq int
	if err := db.QueryRow(`SELECT count(*),max(recipient_sequence) FROM notification_inbox`).Scan(&n, &seq); err != nil || n != 2 || seq != 1 {
		t.Fatalf("duplicate allocated a sequence n%d seq%d %v", n, seq, err)
	}
	var committed int
	if err := db.QueryRow(`SELECT max(committed_sequence) FROM inbox_recipient_state`).Scan(&committed); err != nil || committed != 1 {
		t.Fatalf("committed watermark %d %v", committed, err)
	}
}

func TestExportIsSubjectScopedAndIncludesOnlyRetainedReferences(t *testing.T) {
	db := testDB(t)
	store := NewStore(db)
	ctx := context.Background()
	now := time.Now().UTC()
	for batch := 0; batch < 2; batch++ {
		items := []Write{}
		count := 100
		if batch == 1 {
			count = 1
		}
		for i := 0; i < count; i++ {
			w := write("general", now)
			w.LogicalKey = uuid.NewString()
			items = append(items, w)
		}
		mustAccept(t, store, items...)
	}
	page, err := store.Export(ctx, actor, 0)
	if err != nil || len(page.Items) != 100 || page.NextSequence != 100 || page.Items[0].Resource.Series != "general" {
		t.Fatalf("export %+v %v", page, err)
	}
	last, err := store.Export(ctx, actor, page.NextSequence)
	if err != nil || len(last.Items) != 1 || last.NextSequence != 0 {
		t.Fatalf("continuation %+v %v", last, err)
	}
	other, err := store.Export(ctx, uuid.NewString(), 0)
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("cross subject export %+v %v", other, err)
	}
}
