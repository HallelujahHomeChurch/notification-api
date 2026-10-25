//go:build integration

package worker

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/jackc/pgx/v5"
)

func TestUnclaimedDeliveryDoesNotCheckoutAnotherConnection(t *testing.T) {
	db := workerTestDatabase(t)
	db.SetMaxOpenConns(1)
	until := time.Now().Add(time.Minute)
	id := insertWorkerDelivery(t, db, bytes.Repeat([]byte{1}, 32), statusSending, 1, &until)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, release, err := (postgresStore{db: db}).claimFenced(ctx, id, time.Minute)
	if release != nil {
		defer release()
	}
	if err != nil || result.Claimed || result.Status != statusSending {
		t.Fatalf("active lease status required another pool connection: result=%+v err=%v", result, err)
	}
	if stats := db.Stats(); stats.InUse != 0 || stats.WaitCount != 0 {
		t.Fatalf("unclaimed delivery nested checkout: %+v", stats)
	}
}

func TestDeliveryLockSQLFailureDiscardsSession(t *testing.T) {
	for _, fault := range []string{"acquire", "unlock"} {
		t.Run(fault, func(t *testing.T) {
			db := workerTestDatabase(t)
			db.SetMaxOpenConns(1)
			id := insertWorkerDelivery(t, db, bytes.Repeat([]byte{1}, 32), statusQueued, 0, nil)
			var schema string
			if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			fn, body := "pg_advisory_unlock", `BEGIN RAISE EXCEPTION 'synthetic unlock failure'; END`
			if fault == "acquire" {
				fn, body = "pg_try_advisory_lock", `BEGIN PERFORM pg_catalog.pg_try_advisory_lock($1); RAISE EXCEPTION 'synthetic acquisition failure'; END`
			}
			if _, err := db.Exec(`CREATE FUNCTION ` + pgx.Identifier{schema, fn}.Sanitize() + `(bigint) RETURNS boolean LANGUAGE plpgsql AS $$ ` + body + ` $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`SET search_path TO ` + pgx.Identifier{schema}.Sanitize() + `,pg_catalog`); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			_, release, err := (postgresStore{db: db}).claimFenced(context.Background(), id, time.Minute)
			if fault == "acquire" {
				if err == nil {
					t.Fatal("synthetic lock acquisition succeeded")
				}
			} else {
				if err != nil || release == nil {
					t.Fatalf("claim: %v", err)
				}
				if err := release(); err == nil {
					t.Fatal("synthetic unlock succeeded")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var next int
			if err := db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&next); err != nil {
				t.Fatal(err)
			}
			if next == pid {
				t.Fatal("session with uncertain delivery lock returned to pool")
			}
			var locks int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=$1`, pid).Scan(&locks); err != nil || locks != 0 {
				t.Fatalf("delivery session retained %d locks: %v", locks, err)
			}
		})
	}
}

func TestClaimedDeliveryUsesOnePoolConnection(t *testing.T) {
	db := workerTestDatabase(t)
	db.SetMaxOpenConns(1)
	id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
	provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	message := &fakeMessage{id: id}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := New(db, provider, testKey).Process(ctx, message); err != nil {
		t.Fatalf("claimed delivery nested checkout: %v; pool=%+v", err, db.Stats())
	}
	if provider.calls != 1 || message.completed != 1 || db.Stats().WaitCount != 0 || db.Stats().InUse != 0 {
		t.Fatalf("calls=%d completed=%d pool=%+v", provider.calls, message.completed, db.Stats())
	}
}

func TestClaimedEmailSessionOutcomesAndCancellation(t *testing.T) {
	for _, mode := range []string{"accepted", "temporary", "permanent", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			db := workerTestDatabase(t)
			db.SetMaxOpenConns(1)
			id := insertWorkerDelivery(t, db, testKey, statusQueued, 0, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			provider := &integrationProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
				switch mode {
				case "temporary":
					return providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "mail", SMTPCode: 451}
				case "permanent":
					return providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorPermanent, Operation: "mail", SMTPCode: 550}
				case "canceled":
					cancel()
					return providers.ProviderReceipt{}, context.Canceled
				default:
					return providers.ProviderReceipt{Provider: "smtp", ProviderMessageID: "fixture-receipt"}, nil
				}
			}}
			message := &fakeMessage{id: id}
			w := New(db, provider, testKey).WithEmailLimits(db, EmailLimits{PerMinute: 20, PerHour: 100, ReserveMinute: 5, ReserveHour: 20})
			err := w.Process(ctx, message)
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if provider.calls != 1 {
				t.Fatalf("provider calls=%d", provider.calls)
			}
			var status, outcome string
			if err := db.QueryRow(`SELECT d.status,a.outcome FROM notification_deliveries d JOIN notification_email_attempts a ON a.delivery_id=d.id WHERE d.id=$1`, id).Scan(&status, &outcome); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantOutcome := statusSent, "accepted"
			switch mode {
			case "temporary":
				wantStatus, wantOutcome = statusQueued, "rejected"
			case "permanent":
				wantStatus, wantOutcome = statusFailed, "rejected"
			case "canceled":
				wantStatus, wantOutcome = statusQueued, "unknown"
			}
			if status != wantStatus || outcome != wantOutcome {
				t.Fatalf("status/outcome=%s/%s want%s/%s", status, outcome, wantStatus, wantOutcome)
			}
			var locks int
			if err := db.QueryRow(`SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'`).Scan(&locks); err != nil || locks != 0 {
				t.Fatalf("locks=%d: %v", locks, err)
			}
			if db.Stats().WaitCount != 0 || db.Stats().InUse != 0 {
				t.Fatalf("nested checkout or leaked connection: %+v", db.Stats())
			}
			if w.email.conn != nil {
				t.Fatal("shared email policy retained a claimed session")
			}
			// Reacquire the same physical session's delivery fence after every outcome.
			var acquired, released bool
			if err := db.QueryRow(`SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+id).Scan(&acquired); err != nil || !acquired {
				t.Fatalf("reacquire=%v: %v", acquired, err)
			}
			if err := db.QueryRow(`SELECT pg_advisory_unlock(hashtextextended($1,0))`, "notification-dsr-delivery:"+id).Scan(&released); err != nil || !released {
				t.Fatalf("release=%v: %v", released, err)
			}
		})
	}
}
