package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	HashKeyID       string
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
	RequestHashes     map[string]string
	EncryptionKeyID   string
	HashKeyID         string
	TemplateID        string
	TemplateVersion   int
	Channel           string
	TargetType        string
	TargetHash        string
	TargetHashes      map[string]string
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
	db       database
	hashKeys map[string][]byte
}

func New(db *sql.DB, hashKey []byte) *Store {
	return newStore(sqlDatabase{db: db}, hashKey)
}

func NewWithHashKeys(db *sql.DB, hashKeys map[string][]byte) *Store {
	return newStoreWithHashKeys(sqlDatabase{db: db}, hashKeys)
}

func ValidateKeyReferences(
	ctx context.Context,
	db *sql.DB,
	encryptionKeys map[string][]byte,
	hashKeys map[string][]byte,
) error {
	rows, err := db.QueryContext(ctx, `
		SELECT 'encryption', encryption_key_id
		FROM notification_messages
		WHERE payload_purged_at IS NULL
		GROUP BY encryption_key_id
		UNION ALL
		SELECT 'hash', hash_key_id
		FROM notification_messages
		GROUP BY hash_key_id`)
	if err != nil {
		return fmt.Errorf("read notification key references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, keyID string
		if err := rows.Scan(&kind, &keyID); err != nil {
			return fmt.Errorf("scan notification key reference: %w", err)
		}
		keys := hashKeys
		if kind == "encryption" {
			keys = encryptionKeys
		}
		if keys != nil {
			if _, ok := keys[keyID]; !ok {
				return fmt.Errorf("notification %s key %q is still referenced", kind, keyID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate notification key references: %w", err)
	}
	return nil
}

func newStore(db database, hashKey []byte) *Store {
	return newStoreWithHashKeys(db, map[string][]byte{"legacy-v1": hashKey})
}

func newStoreWithHashKeys(db database, hashKeys map[string][]byte) *Store {
	return &Store{db: db, hashKeys: hashKeys}
}

func (s *Store) Create(ctx context.Context, params CreateParams) (CreateResult, error) {
	if params.HashKeyID == "" {
		if _, ok := s.hashKeys["legacy-v1"]; !ok {
			return CreateResult{}, fmt.Errorf("notification hash key ID is required")
		}
		params.HashKeyID = "legacy-v1"
	}
	if params.EncryptionKeyID == "" {
		params.EncryptionKeyID = "legacy-v1"
	}
	if params.RequestHashes == nil {
		params.RequestHashes = map[string]string{params.HashKeyID: params.RequestHash}
	}
	if params.TargetHashes == nil {
		params.TargetHashes = map[string]string{params.HashKeyID: params.TargetHash}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateResult{}, fmt.Errorf("begin notification intent: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		idempotencyLockKey(params.Caller, params.IdempotencyKey),
	); err != nil {
		return CreateResult{}, fmt.Errorf("lock notification idempotency key: %w", err)
	}

	existing, err := findByIdempotency(ctx, tx, params.Caller, params.IdempotencyKey)
	switch {
	case err == nil:
		return resolveExisting(existing, params.RequestHashes)
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
				resource_type, resource_id, encryption_key_id, hash_key_id, status
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'queued')`,
		params.MessageID, params.Caller, params.IdempotencyKey, params.RequestHash,
		params.TemplateID, params.TemplateVersion, params.Channel, params.TargetType,
		params.TargetHash, params.TargetCiphertext, params.PayloadCiphertext,
		params.ResourceType, params.ResourceID, params.EncryptionKeyID, params.HashKeyID,
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
			HashKeyID:       params.HashKeyID,
			TemplateVersion: params.TemplateVersion,
			Status:          contracts.MessageStatusQueued,
		},
	}, nil
}

func (s *Store) Get(ctx context.Context, caller, messageID string) (Message, error) {
	var message Message
	err := s.db.QueryRowContext(ctx, `
			SELECT id, caller_app_id, request_hash, hash_key_id, template_version, status
		FROM notification_messages
		WHERE id=$1 AND caller_app_id=$2`,
		messageID, caller,
	).Scan(
		&message.ID,
		&message.Caller,
		&message.RequestHash,
		&message.HashKeyID,
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
	keyIDs := make([]string, 0, len(s.hashKeys))
	for keyID := range s.hashKeys {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	for _, limit := range params.RateLimits {
		if limit.Window < time.Second || limit.Window%time.Second != 0 || limit.Maximum <= 0 {
			return 0, fmt.Errorf("invalid rate limit")
		}
		windowSeconds := int64(limit.Window / time.Second)
		windowStart := time.Unix(databaseNow.Unix()/windowSeconds*windowSeconds, 0).UTC()
		expiresAt := windowStart.Add(limit.Window)
		for _, keyID := range keyIDs {
			targetHash, ok := params.TargetHashes[keyID]
			if !ok {
				return 0, fmt.Errorf("target hash key %q is missing", keyID)
			}
			key := rateBucketKey(
				s.hashKeys[keyID],
				params.Caller,
				params.TemplateID,
				targetHash,
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
	}
	return 0, nil
}

func (s *Store) resolveUniqueRace(ctx context.Context, params CreateParams) (CreateResult, error) {
	message, err := findByIdempotency(ctx, s.db, params.Caller, params.IdempotencyKey)
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve notification idempotency race: %w", err)
	}
	return resolveExisting(message, params.RequestHashes)
}

func findByIdempotency(ctx context.Context, query queryer, caller, idempotencyKey string) (Message, error) {
	var message Message
	err := query.QueryRowContext(ctx, `
		SELECT id, caller_app_id, request_hash, hash_key_id, template_version, status
		FROM notification_messages
		WHERE caller_app_id=$1 AND idempotency_key=$2`,
		caller, idempotencyKey,
	).Scan(
		&message.ID,
		&message.Caller,
		&message.RequestHash,
		&message.HashKeyID,
		&message.TemplateVersion,
		&message.Status,
	)
	return message, err
}

func resolveExisting(message Message, requestHashes map[string]string) (CreateResult, error) {
	requestHash, ok := requestHashes[message.HashKeyID]
	if !ok {
		return CreateResult{}, fmt.Errorf("notification hash key %q is not configured", message.HashKeyID)
	}
	if message.RequestHash != requestHash {
		return CreateResult{Conflict: true}, nil
	}
	return CreateResult{Message: message, Replayed: true}, nil
}

func idempotencyLockKey(caller, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(caller + "\x00" + idempotencyKey))
	return fmt.Sprintf("%x", sum)
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
