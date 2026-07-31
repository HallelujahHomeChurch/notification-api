package database

import (
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
)

func TestOpenAppliesConfiguredMaxOpenConnections(t *testing.T) {
	db, err := Open(config.Config{
		DatabaseURL:       "postgres://notification:password@localhost:5432/notification",
		DBMaxOpenConns:    7,
		DBMaxIdleConns:    3,
		DBConnMaxLifetime: 20 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7", got)
	}
}

func TestOpenRejectsInvalidPoolConfig(t *testing.T) {
	tests := []config.Config{
		{DatabaseURL: "postgres://notification:password@localhost:5432/notification"},
		{
			DatabaseURL:       "postgres://notification:password@localhost:5432/notification",
			DBMaxOpenConns:    -1,
			DBMaxIdleConns:    1,
			DBConnMaxLifetime: time.Minute,
		},
		{
			DatabaseURL:       "postgres://notification:password@localhost:5432/notification",
			DBMaxOpenConns:    1,
			DBMaxIdleConns:    2,
			DBConnMaxLifetime: time.Minute,
		},
	}
	for _, cfg := range tests {
		if db, err := Open(cfg); err == nil {
			db.Close()
			t.Fatalf("Open(%+v) error = nil", cfg)
		}
	}
}
