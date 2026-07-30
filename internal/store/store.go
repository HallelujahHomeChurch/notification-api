package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/jackc/pgx/v5/pgconn"
)

type Message struct {
	ID              string
	Caller          string
	RequestHash     string
	TemplateVersion int
	Status          contracts.MessageStatus
}

type RateLimit struct {
	Window       time.Duration
	Maximum      int
	TemplateWide bool
}

type CreateParams struct {
	MessageID         string
	DeliveryID        string
	OutboxID          string
	Caller            string
	IdempotencyKey    string
	RequestHash       string
	TemplateID        string
	TemplateVersion   int
	Channel           string
	TargetType        string
	TargetHash        string
	TargetCiphertext  []byte
	PayloadCiphertext []byte
	ResourceType      string
	ResourceID        string
	Provider          string
	RateLimits        []RateLimit
}

type CreateResult struct {
	Message    Message
	Replayed   bool
	Conflict   bool
	RetryAfter time.Duration
}

type row interface {
	Scan(...any) error
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) row
}

type transaction interface {
	queryer
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

type database interface {
	queryer
	BeginTx(context.Context, *sql.TxOptions) (transaction, error)
}

type sqlDatabase struct {
	db *sql.DB
}

func (db sqlDatabase) BeginTx(ctx context.Context, options *sql.TxOptions) (transaction, error) {
	tx, err := db.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return sqlTransaction{tx: tx}, nil
}

func (db sqlDatabase) QueryRowContext(ctx context.Context, query string, args ...any) row {
	return db.db.QueryRowContext(ctx, query, args...)
}

type sqlTransaction struct {
	tx *sql.Tx
}

func (tx sqlTransaction) QueryRowContext(ctx context.Context, query string, args ...any) row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}

func (tx sqlTransaction) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

func (tx sqlTransaction) Commit() error {
	return tx.tx.Commit()
}

func (tx sqlTransaction) Rollback() error {
	return tx.tx.Rollback()
}

type Store struct {
	db      database
	hashKey []byte
}

func New(db *sql.DB, hashKey []byte) *Store {
	return newStore(sqlDatabase{db: db}, hashKey)
}

func newStore(db database, hashKey []byte) *Store {
	return &Store{db: db, hashKey: hashKey}
}

func (s *Store) Create(ctx context.Context, params CreateParams) (CreateResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateResult{}, fmt.Errorf("begin notification intent: %w", err)
	}
	defer tx.Rollback()

	lockKey := notificationcrypto.Hash(
		s.hashKey,
		[]byte(params.Caller+"\x00"+params.IdempotencyKey),
	)
	if _, err := tx.ExecContext(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		lockKey,
	); err != nil {
		return CreateResult{}, fmt.Errorf("lock notification idempotency key: %w", err)
	}

	existing, err := findByIdempotency(ctx, tx, params.Caller, params.IdempotencyKey)
	switch {
	case err == nil:
		return resolveExisting(existing, params.RequestHash), nil
	case !errors.Is(err, sql.ErrNoRows):
		return CreateResult{}, fmt.Errorf("find notification intent: %w", err)
	}

	retryAfter, err := s.applyRateLimits(ctx, tx, params)
	if err != nil {
		return CreateResult{}, err
	}
	if retryAfter > 0 {
		return CreateResult{RetryAfter: retryAfter}, nil
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_messages (
			id, caller_app_id, idempotency_key, request_hash, template_id, template_version,
			channel, target_type, target_hash, target_ciphertext, payload_ciphertext,
			resource_type, resource_id, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'queued')`,
		params.MessageID, params.Caller, params.IdempotencyKey, params.RequestHash,
		params.TemplateID, params.TemplateVersion, params.Channel, params.TargetType,
		params.TargetHash, params.TargetCiphertext, params.PayloadCiphertext,
		params.ResourceType, params.ResourceID,
	); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback()
			return s.resolveUniqueRace(ctx, params)
		}
		return CreateResult{}, fmt.Errorf("insert notification message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_deliveries (
			id, message_id, channel, provider, status
		) VALUES ($1,$2,$3,$4,'queued')`,
		params.DeliveryID, params.MessageID, params.Channel, params.Provider,
	); err != nil {
		return CreateResult{}, fmt.Errorf("insert notification delivery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_outbox (id, delivery_id, status)
		VALUES ($1,$2,'pending')`,
		params.OutboxID, params.DeliveryID,
	); err != nil {
		return CreateResult{}, fmt.Errorf("insert notification outbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CreateResult{}, fmt.Errorf("commit notification intent: %w", err)
	}
	return CreateResult{
		Message: Message{
			ID:              params.MessageID,
			Caller:          params.Caller,
			RequestHash:     params.RequestHash,
			TemplateVersion: params.TemplateVersion,
			Status:          contracts.MessageStatusQueued,
		},
	}, nil
}

func (s *Store) Get(ctx context.Context, caller, messageID string) (Message, error) {
	var message Message
	err := s.db.QueryRowContext(ctx, `
		SELECT id, caller_app_id, request_hash, template_version, status
		FROM notification_messages
		WHERE id=$1 AND caller_app_id=$2`,
		messageID, caller,
	).Scan(
		&message.ID,
		&message.Caller,
		&message.RequestHash,
		&message.TemplateVersion,
		&message.Status,
	)
	return message, err
}

func (s *Store) applyRateLimits(ctx context.Context, tx transaction, params CreateParams) (time.Duration, error) {
	if len(params.RateLimits) == 0 {
		return 0, nil
	}
	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&databaseNow); err != nil {
		return 0, fmt.Errorf("read database clock: %w", err)
	}
	for _, limit := range params.RateLimits {
		if limit.Window < time.Second || limit.Window%time.Second != 0 || limit.Maximum <= 0 {
			return 0, fmt.Errorf("invalid rate limit")
		}
		windowSeconds := int64(limit.Window / time.Second)
		windowStart := time.Unix(databaseNow.Unix()/windowSeconds*windowSeconds, 0).UTC()
		expiresAt := windowStart.Add(limit.Window)
		key := rateBucketKey(
			s.hashKey,
			params.Caller,
			params.TemplateID,
			params.TargetHash,
			limit,
			windowStart,
		)
		var count, retryAfterSeconds int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO notification_rate_limits (bucket_key, count, expires_at)
			VALUES ($1,1,$2)
			ON CONFLICT (bucket_key) DO UPDATE
			SET count=notification_rate_limits.count+1,
			    expires_at=EXCLUDED.expires_at
			RETURNING count,
			          GREATEST(
			              CEIL(EXTRACT(EPOCH FROM (expires_at-clock_timestamp()))),
			              1
			          )::bigint`,
			key, expiresAt,
		).Scan(&count, &retryAfterSeconds); err != nil {
			return 0, fmt.Errorf("apply notification rate limit: %w", err)
		}
		if count > int64(limit.Maximum) {
			return time.Duration(retryAfterSeconds) * time.Second, nil
		}
	}
	return 0, nil
}

func (s *Store) resolveUniqueRace(ctx context.Context, params CreateParams) (CreateResult, error) {
	message, err := findByIdempotency(ctx, s.db, params.Caller, params.IdempotencyKey)
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve notification idempotency race: %w", err)
	}
	return resolveExisting(message, params.RequestHash), nil
}

func findByIdempotency(ctx context.Context, query queryer, caller, idempotencyKey string) (Message, error) {
	var message Message
	err := query.QueryRowContext(ctx, `
		SELECT id, caller_app_id, request_hash, template_version, status
		FROM notification_messages
		WHERE caller_app_id=$1 AND idempotency_key=$2`,
		caller, idempotencyKey,
	).Scan(
		&message.ID,
		&message.Caller,
		&message.RequestHash,
		&message.TemplateVersion,
		&message.Status,
	)
	return message, err
}

func resolveExisting(message Message, requestHash string) CreateResult {
	if message.RequestHash != requestHash {
		return CreateResult{Conflict: true}
	}
	return CreateResult{Message: message, Replayed: true}
}

func rateBucketKey(
	hashKey []byte,
	caller string,
	templateID string,
	targetHash string,
	limit RateLimit,
	windowStart time.Time,
) string {
	if limit.TemplateWide {
		targetHash = ""
	}
	value := strings.Join([]string{
		caller,
		templateID,
		targetHash,
		strconv.FormatInt(int64(limit.Window/time.Second), 10),
		strconv.FormatInt(windowStart.Unix(), 10),
	}, "\x00")
	return notificationcrypto.Hash(hashKey, []byte(value))
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
