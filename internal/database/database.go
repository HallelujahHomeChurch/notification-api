package database

import (
	"database/sql"
	"fmt"
	"github.com/HallelujahHomeChurch/notification-api/internal/dbphase"
	"github.com/HallelujahHomeChurch/notification-api/internal/telemetry"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func Open(cfg config.Config) (*sql.DB, error)        { return open(cfg, false) }
func OpenRuntime(cfg config.Config) (*sql.DB, error) { return open(cfg, true) }
func open(cfg config.Config, runtime bool) (*sql.DB, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.DBMaxOpenConns <= 0 {
		return nil, fmt.Errorf("DB_MAX_OPEN_CONNS must be positive")
	}
	if cfg.DBMaxIdleConns <= 0 {
		return nil, fmt.Errorf("DB_MAX_IDLE_CONNS must be positive")
	}
	if cfg.DBMaxIdleConns > cfg.DBMaxOpenConns {
		return nil, fmt.Errorf("DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS")
	}
	if cfg.DBConnMaxLifetime <= 0 {
		return nil, fmt.Errorf("DB_CONN_MAX_LIFETIME must be positive")
	}
	var db *sql.DB
	var err error
	if runtime {
		db, err = dbphase.OpenURL(cfg.DatabaseURL, telemetry.RecordDatabaseOutcome)
	} else {
		db, err = sql.Open("pgx", cfg.DatabaseURL)
	}
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	return db, nil
}
