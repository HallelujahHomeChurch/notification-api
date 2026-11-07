package database

import (
	"context"
	"github.com/HallelujahHomeChurch/notification-api/internal/dbphase"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestObservedPoolPreservesNativeSessionTransactions(t *testing.T) {
	dsn := os.Getenv("DB_PHASE_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Database != "db_phase_test" && cfg.Database != "notification_test") || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("requires isolated loopback fixture")
	}
	db := dbphase.Open(*cfg, func(context.Context, string, time.Duration, bool) {})
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		tx, err := BeginSessionTransaction(ctx, conn)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var value int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
			cancel()
			t.Fatal(err)
		}
		if cancelled {
			cancel()
			err = tx.Rollback()
		} else {
			err = tx.Commit()
			cancel()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.PingContext(t.Context()); err != nil {
		t.Fatal("session cleanup left connection unusable", err)
	}
}
