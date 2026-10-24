//go:build integration

package worker

import (
	"bytes"
	"context"
	"testing"
	"time"

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
