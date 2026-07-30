package store

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		value := r.values[i]
		target := reflect.ValueOf(dest[i]).Elem()
		source := reflect.ValueOf(value)
		if source.Type().ConvertibleTo(target.Type()) {
			target.Set(source.Convert(target.Type()))
		} else {
			target.Set(source)
		}
	}
	return nil
}

type fakeExec struct {
	query string
	args  []any
}

type fakeTransaction struct {
	rows       []fakeRow
	queries    []fakeExec
	execs      []fakeExec
	execErrors map[string]error
	committed  bool
	rolledBack bool
}

func (tx *fakeTransaction) QueryRowContext(_ context.Context, query string, args ...any) row {
	tx.queries = append(tx.queries, fakeExec{query: query, args: args})
	result := tx.rows[0]
	tx.rows = tx.rows[1:]
	return result
}

func (tx *fakeTransaction) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	tx.execs = append(tx.execs, fakeExec{query: query, args: args})
	for fragment, err := range tx.execErrors {
		if strings.Contains(query, fragment) {
			return nil, err
		}
	}
	return nil, nil
}

func (tx *fakeTransaction) Commit() error {
	tx.committed = true
	return nil
}

func (tx *fakeTransaction) Rollback() error {
	if !tx.committed {
		tx.rolledBack = true
	}
	return nil
}

type fakeDatabase struct {
	tx          *fakeTransaction
	outsideRows []fakeRow
	beginCalls  int
}

func (db *fakeDatabase) BeginTx(context.Context, *sql.TxOptions) (transaction, error) {
	db.beginCalls++
	return db.tx, nil
}

func (db *fakeDatabase) QueryRowContext(_ context.Context, _ string, _ ...any) row {
	result := db.outsideRows[0]
	db.outsideRows = db.outsideRows[1:]
	return result
}

func TestCreatePersistsMessageDeliveryAndOutboxInOneTransaction(t *testing.T) {
	tx := &fakeTransaction{rows: []fakeRow{{err: sql.ErrNoRows}}}
	db := &fakeDatabase{tx: tx}
	store := newStore(db, []byte("hash-key"))

	result, err := store.Create(context.Background(), createParams())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.Message.ID != "message-1" || result.Replayed || result.Conflict {
		t.Fatalf("Create() = %#v", result)
	}
	if db.beginCalls != 1 || !tx.committed || tx.rolledBack {
		t.Fatalf("transaction begin=%d committed=%v rolledBack=%v", db.beginCalls, tx.committed, tx.rolledBack)
	}
	if len(tx.execs) != 4 {
		t.Fatalf("transaction statements = %d, want lock plus 3 writes", len(tx.execs))
	}
	if !strings.Contains(tx.execs[0].query, "pg_advisory_xact_lock") {
		t.Fatalf("first transaction statement = %q, want idempotency lock", tx.execs[0].query)
	}
	for index, table := range []string{"notification_messages", "notification_deliveries", "notification_outbox"} {
		if !strings.Contains(tx.execs[index+1].query, table) {
			t.Fatalf("write %d query = %q, want %s", index, tx.execs[index+1].query, table)
		}
	}
}

func TestCreateRateLimitUsesDatabaseClockAndWritesNoIntent(t *testing.T) {
	databaseNow := time.Date(2026, 7, 27, 4, 30, 45, 0, time.UTC)
	tx := &fakeTransaction{rows: []fakeRow{
		{err: sql.ErrNoRows},
		{values: []any{databaseNow}},
		{values: []any{int64(2), int64(1)}},
	}}
	db := &fakeDatabase{tx: tx}
	store := newStore(db, []byte("hash-key"))
	params := createParams()
	params.RateLimits = []RateLimit{{Window: time.Minute, Maximum: 1}}

	result, err := store.Create(context.Background(), params)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.RetryAfter != time.Second {
		t.Fatalf("Create().RetryAfter = %s, want fresh clamped 1s", result.RetryAfter)
	}
	rateQuery := strings.ToLower(tx.queries[2].query)
	if !strings.Contains(rateQuery, "clock_timestamp()") || !strings.Contains(rateQuery, "greatest") {
		t.Fatalf("rate-limit query = %q, want fresh clamped PostgreSQL clock", tx.queries[2].query)
	}
	if len(tx.execs) != 1 || !strings.Contains(tx.execs[0].query, "pg_advisory_xact_lock") {
		t.Fatalf("statements=%#v, want only idempotency lock", tx.execs)
	}
	if !tx.rolledBack || tx.committed {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestCreateResolvesUniqueKeyRaceDeterministically(t *testing.T) {
	for _, test := range []struct {
		name         string
		winnerHash   string
		wantReplayed bool
		wantConflict bool
	}{
		{name: "same hash replays winner", winnerHash: "request-hash", wantReplayed: true},
		{name: "different hash conflicts", winnerHash: "other-hash", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := &fakeTransaction{
				rows: []fakeRow{{err: sql.ErrNoRows}},
				execErrors: map[string]error{
					"notification_messages": &pgconn.PgError{Code: "23505"},
				},
			}
			db := &fakeDatabase{
				tx: tx,
				outsideRows: []fakeRow{{values: []any{
					"winner-message", "account-api", test.winnerHash, 1, contracts.MessageStatusQueued,
				}}},
			}
			store := newStore(db, []byte("hash-key"))

			result, err := store.Create(context.Background(), createParams())
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if result.Replayed != test.wantReplayed || result.Conflict != test.wantConflict {
				t.Fatalf("Create() = %#v", result)
			}
			if !tx.rolledBack || tx.committed {
				t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
			}
		})
	}
}

func TestRateBucketKeyDoesNotContainRecipient(t *testing.T) {
	key := rateBucketKey(
		[]byte("hash-key"),
		"account-api",
		"account.verify-email",
		"target-hash",
		RateLimit{Window: time.Minute, Maximum: 1},
		time.Unix(1_721_000_000, 0),
	)
	for _, plaintext := range []string{"account-api", "account.verify-email", "target-hash"} {
		if strings.Contains(key, plaintext) {
			t.Fatalf("rateBucketKey() = %q contains %q", key, plaintext)
		}
	}
	if len(key) != 64 {
		t.Fatalf("rateBucketKey() length = %d, want 64", len(key))
	}
}

func TestRateBucketKeyScopesTemplateLimitAcrossRecipients(t *testing.T) {
	start := time.Unix(1_721_000_000, 0)
	recipientLimit := RateLimit{Window: 24 * time.Hour, Maximum: 5}
	templateLimit := RateLimit{Window: 24 * time.Hour, Maximum: 1_000, TemplateWide: true}

	recipientA := rateBucketKey(
		[]byte("hash-key"), "account-api", "account.verify-email", "target-a", recipientLimit, start,
	)
	recipientB := rateBucketKey(
		[]byte("hash-key"), "account-api", "account.verify-email", "target-b", recipientLimit, start,
	)
	templateA := rateBucketKey(
		[]byte("hash-key"), "account-api", "account.verify-email", "target-a", templateLimit, start,
	)
	templateB := rateBucketKey(
		[]byte("hash-key"), "account-api", "account.verify-email", "target-b", templateLimit, start,
	)

	if recipientA == recipientB {
		t.Fatal("recipient-scoped rate-limit keys must differ")
	}
	if templateA != templateB {
		t.Fatal("template-wide rate-limit keys must match across recipients")
	}
}

func createParams() CreateParams {
	return CreateParams{
		MessageID:        "message-1",
		DeliveryID:       "delivery-1",
		OutboxID:         "outbox-1",
		Caller:           "account-api",
		IdempotencyKey:   "request-1",
		RequestHash:      "request-hash",
		TemplateID:       "account.verify-email",
		TemplateVersion:  1,
		Channel:          "email",
		TargetType:       "email",
		TargetHash:       "target-hash",
		TargetCiphertext: []byte("encrypted-target"),
		PayloadCiphertext: []byte(
			"encrypted-payload",
		),
		ResourceType: "account",
		ResourceID:   "user-1",
		Provider:     "smtp",
	}
}
