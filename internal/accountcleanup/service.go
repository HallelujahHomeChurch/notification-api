package accountcleanup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/google/uuid"
)

const owner = "notification-api"

var ErrInvalidRequest = errors.New("invalid account cleanup request")

type Service struct {
	db       *sql.DB
	hashKeys map[string][]byte
}

func New(db *sql.DB, hashKeys map[string][]byte) *Service {
	return &Service{db: db, hashKeys: hashKeys}
}

type AccountCleanupRequest struct {
	UserID         string `json:"userId"`
	Email          string `json:"canonicalEmail"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type AccountCleanupResult struct {
	Owner          string   `json:"owner"`
	Status         string   `json:"status"`
	RecordCount    int64    `json:"recordCount"`
	RemainingCount int64    `json:"remainingCount"`
	ReasonCodes    []string `json:"reasonCodes"`
}

func (s *Service) CleanupAccount(ctx context.Context, request AccountCleanupRequest) (AccountCleanupResult, error) {
	if _, err := uuid.Parse(request.UserID); err != nil || !contracts.ValidEmail(request.Email) || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 200 {
		return AccountCleanupResult{}, ErrInvalidRequest
	}
	result, err := s.eraseIdempotent(ctx, request.UserID, request.Email, request.IdempotencyKey)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	return result, nil
}

func (s *Service) eraseIdempotent(ctx context.Context, userID, email, idempotencyKey string) (AccountCleanupResult, error) {
	parsedUserID, err := uuid.Parse(userID)
	if err != nil {
		return AccountCleanupResult{}, ErrInvalidRequest
	}
	userID = parsedUserID.String()
	emailKeyIDs, emailHashes := candidateHashes(s.hashKeys, email)
	subjectKeyIDs, subjectHashes := candidateSubjectHashes(s.hashKeys, userID)
	if len(emailKeyIDs) == 0 {
		return AccountCleanupResult{}, ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		subjectHash := sha256.Sum256([]byte("notification-account-cleanup:" + userID))
		subjectRef := hex.EncodeToString(subjectHash[:])
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "notification-account-subject:"+userID); err != nil {
			return AccountCleanupResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_cleanup_operations(idempotency_key,subject_ref) VALUES($1,$2) ON CONFLICT DO NOTHING`, idempotencyKey, subjectRef); err != nil {
			return AccountCleanupResult{}, err
		}
		var operationSubject string
		var stored []byte
		if err := tx.QueryRowContext(ctx, `SELECT subject_ref,result FROM account_cleanup_operations WHERE idempotency_key=$1 FOR UPDATE`, idempotencyKey).Scan(&operationSubject, &stored); err != nil {
			return AccountCleanupResult{}, err
		}
		if operationSubject != subjectRef {
			return AccountCleanupResult{}, ErrInvalidRequest
		}
		if len(stored) != 0 {
			var result AccountCleanupResult
			if err := json.Unmarshal(stored, &result); err != nil {
				return AccountCleanupResult{}, err
			}
			return result, tx.Commit()
		}
	}
	if err := lockDeliveries(ctx, tx, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes); err != nil {
		return AccountCleanupResult{}, err
	}
	if _, err := tx.ExecContext(ctx, suppressOutboxQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes); err != nil {
		return AccountCleanupResult{}, err
	}
	if _, err := tx.ExecContext(ctx, suppressDeliveriesQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes); err != nil {
		return AccountCleanupResult{}, err
	}
	erasedSuppressions, err := tx.ExecContext(ctx, eraseSuppressionsQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	n, err := erasedSuppressions.RowsAffected()
	if err != nil {
		return AccountCleanupResult{}, err
	}
	result, err := tx.ExecContext(ctx, eraseQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AccountCleanupResult{}, err
	}
	affected += n
	var remaining, unattributedLegacy int64
	if err := tx.QueryRowContext(ctx, postconditionQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes).Scan(&remaining); err != nil {
		return AccountCleanupResult{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notification_messages WHERE caller_app_id IN ('account-api','engagement-api') AND subject_hash IS NULL AND target_hash<>repeat('0',64)`).Scan(&unattributedLegacy); err != nil {
		return AccountCleanupResult{}, err
	}
	status := "completed"
	if remaining != 0 {
		status = "pending"
	}
	reasons := []string{}
	if unattributedLegacy != 0 {
		reasons = append(reasons, "legacy_unattributed_notifications")
	}
	cleanupResult := AccountCleanupResult{Owner: owner, Status: status, RecordCount: affected, RemainingCount: remaining, ReasonCodes: reasons}
	if idempotencyKey != "" && cleanupResult.Status == "completed" {
		encoded, err := json.Marshal(cleanupResult)
		if err != nil {
			return AccountCleanupResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_cleanup_operations SET result=$2,updated_at=now() WHERE idempotency_key=$1`, idempotencyKey, encoded); err != nil {
			return AccountCleanupResult{}, err
		}
	}
	return cleanupResult, tx.Commit()
}

func lockDeliveries(ctx context.Context, tx *sql.Tx, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes []string) error {
	rows, err := tx.QueryContext(ctx, `
		WITH subject_candidates AS (
			SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
		), email_candidates AS (
			SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
		)
		SELECT delivery.id
		FROM notification_deliveries AS delivery
		JOIN notification_messages AS message ON message.id=delivery.message_id
		WHERE EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
		   OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash))
		ORDER BY delivery.id`, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+id); err != nil {
			return err
		}
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT delivery.id
		FROM notification_deliveries AS delivery
		JOIN notification_messages AS message ON message.id=delivery.message_id
		WHERE delivery.id=ANY($1::uuid[])
		ORDER BY delivery.id
		FOR UPDATE OF delivery, message`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func candidateHashes(hashKeys map[string][]byte, email string) ([]string, []string) {
	email = strings.ToLower(strings.TrimSpace(email))
	keyIDs := make([]string, 0, len(hashKeys))
	for keyID := range hashKeys {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	hashes := make([]string, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		hashes = append(hashes, notificationcrypto.Hash(hashKeys[keyID], []byte(email)))
	}
	return keyIDs, hashes
}

func candidateSubjectHashes(hashKeys map[string][]byte, userID string) ([]string, []string) {
	parsed, err := uuid.Parse(userID)
	if err != nil {
		return nil, nil
	}
	return candidateHashes(hashKeys, "notification-account-subject:"+parsed.String())
}

const eraseQuery = `
	WITH subject_candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
	), email_candidates AS (
		SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
	)
	UPDATE notification_messages AS message
	SET target_ciphertext=''::bytea,
		payload_ciphertext=''::bytea,
		target_hash=repeat('0',64),
		target_hashes='{}'::jsonb,
		request_hash=repeat('0',64),
		idempotency_key='erased:'||message.id::text,
		resource_id='',
		eligibility_campaign_id=NULL,
		eligibility_recipient_id=NULL,
		subject_hash=NULL,
		subject_hash_key_id=NULL,
		status=CASE WHEN message.status IN ('queued','sending') THEN 'suppressed' ELSE message.status END,
		terminal_at=CASE WHEN message.status IN ('queued','sending') THEN COALESCE(message.terminal_at,clock_timestamp()) ELSE message.terminal_at END,
		payload_purged_at=COALESCE(payload_purged_at, clock_timestamp()),
		updated_at=clock_timestamp()
	WHERE message.target_hash<>repeat('0',64)
	  AND (
	    EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
	    OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash))
	  )`

const suppressOutboxQuery = `
	WITH subject_candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
	), email_candidates AS (
		SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
	), messages AS (
		SELECT message.id FROM notification_messages message
		WHERE EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
		   OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash))
	)
	DELETE FROM notification_outbox outbox
	USING notification_deliveries delivery, messages
	WHERE outbox.delivery_id=delivery.id AND delivery.message_id=messages.id AND outbox.status<>'published'`

const suppressDeliveriesQuery = `
	WITH subject_candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
	), email_candidates AS (
		SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
	), messages AS (
		SELECT message.id FROM notification_messages message
		WHERE EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
		   OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash))
	)
	UPDATE notification_deliveries delivery
	SET status='suppressed',lease_expires_at=NULL,last_error_code='account_erased',updated_at=clock_timestamp()
	FROM messages
	WHERE delivery.message_id=messages.id AND delivery.status IN ('queued','sending','failed')`

const postconditionQuery = `
	WITH subject_candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
	), email_candidates AS (
		SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
	)
	SELECT count(*)
	FROM notification_messages AS message
	WHERE EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
	   OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash))`

// Erase reachable historical aliases before tombstoning the Account-bound messages.
const eraseSuppressionsQuery = `
 WITH subject_candidates AS (
  SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
 ), email_candidates AS (
  SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
 ), matched AS (
  SELECT message.* FROM notification_messages message WHERE message.channel='email' AND (
   EXISTS (SELECT 1 FROM subject_candidates c WHERE c.hash_key_id=message.subject_hash_key_id AND c.subject_hash=message.subject_hash)
   OR (message.subject_hash IS NULL AND EXISTS (SELECT 1 FROM email_candidates c WHERE c.hash_key_id=message.hash_key_id AND c.target_hash=message.target_hash)))
 ), aliases AS (
  SELECT candidate.key AS hash_key_id,candidate.value AS target_hash FROM matched m
  CROSS JOIN LATERAL jsonb_each_text(m.target_hashes || jsonb_build_object(m.hash_key_id,m.target_hash)) candidate
  UNION SELECT hash_key_id,target_hash FROM email_candidates
 )
 DELETE FROM notification_email_suppressions s USING aliases a WHERE s.hash_key_id=a.hash_key_id AND s.target_hash=a.target_hash`
