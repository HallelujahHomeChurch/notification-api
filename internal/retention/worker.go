package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	payloadRetention  = 7 * 24 * time.Hour
	metadataRetention = 730 * 24 * time.Hour
	defaultBatchSize  = 100
	defaultInterval   = time.Hour
)

type Result struct {
	Tombstoned         int
	MessagesDeleted    int
	RateBucketsDeleted int
}

type repository interface {
	retain(context.Context, int, time.Duration, time.Duration) (Result, error)
}

type postgresRepository struct {
	db *sql.DB
}

type Worker struct {
	repository repository
	batchSize  int
	interval   time.Duration
}

func New(db *sql.DB) *Worker {
	return newWorker(postgresRepository{db: db})
}

func newWorker(repository repository) *Worker {
	return &Worker{
		repository: repository,
		batchSize:  defaultBatchSize,
		interval:   defaultInterval,
	}
}

func (w *Worker) RunOnce(ctx context.Context) (Result, error) {
	return w.repository.retain(ctx, w.batchSize, payloadRetention, metadataRetention)
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		result, err := w.RunOnce(ctx)
		if err != nil {
			return err
		}
		if result.Tombstoned+result.MessagesDeleted+result.RateBucketsDeleted > 0 {
			continue
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (r postgresRepository) retain(
	ctx context.Context,
	batchSize int,
	payloadAge time.Duration,
	metadataAge time.Duration,
) (Result, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin retention batch: %w", err)
	}
	defer tx.Rollback()

	deleted, err := execBatch(ctx, tx, `
		WITH leased AS (
			SELECT id
			FROM notification_messages
			WHERE terminal_at <= clock_timestamp()-($1::bigint*interval '1 microsecond')
			ORDER BY terminal_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		DELETE FROM notification_messages AS message
		USING leased
		WHERE message.id=leased.id`,
		metadataAge.Microseconds(),
		batchSize,
	)
	if err != nil {
		return Result{}, fmt.Errorf("delete notification metadata: %w", err)
	}

	tombstoned, err := execBatch(ctx, tx, `
		WITH leased AS (
			SELECT id
			FROM notification_messages
			WHERE terminal_at <= clock_timestamp()-($1::bigint*interval '1 microsecond')
			  AND payload_purged_at IS NULL
			ORDER BY terminal_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE notification_messages AS message
		SET target_ciphertext=''::bytea,
		    payload_ciphertext=''::bytea,
		    payload_purged_at=clock_timestamp(),
		    updated_at=clock_timestamp()
		FROM leased
		WHERE message.id=leased.id`,
		payloadAge.Microseconds(),
		batchSize,
	)
	if err != nil {
		return Result{}, fmt.Errorf("tombstone notification payload: %w", err)
	}

	rateBucketsDeleted, err := execBatch(ctx, tx, `
		WITH leased AS (
			SELECT bucket_key
			FROM notification_rate_limits
			WHERE expires_at <= clock_timestamp()
			ORDER BY expires_at, bucket_key
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		DELETE FROM notification_rate_limits AS bucket
		USING leased
		WHERE bucket.bucket_key=leased.bucket_key`,
		batchSize,
	)
	if err != nil {
		return Result{}, fmt.Errorf("delete notification rate buckets: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit retention batch: %w", err)
	}
	return Result{
		Tombstoned:         tombstoned,
		MessagesDeleted:    deleted,
		RateBucketsDeleted: rateBucketsDeleted,
	}, nil
}

func execBatch(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}
