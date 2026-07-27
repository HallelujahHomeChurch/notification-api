package database

import (
	"database/sql"
	"fmt"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func Open(cfg config.Config) (*sql.DB, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return sql.Open("pgx", cfg.DatabaseURL)
}
