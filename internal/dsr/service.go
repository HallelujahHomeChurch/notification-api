package dsr

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"sort"
	"strings"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/google/uuid"
)

const owner = "notification-api"

var ErrInvalidRequest = errors.New("invalid DSR request")

type Service struct {
	db       *sql.DB
	hashKeys map[string][]byte
}

func New(db *sql.DB, hashKeys map[string][]byte) *Service {
	return &Service{db: db, hashKeys: hashKeys}
}

type ExportRequest struct {
	RequestID string `json:"requestId"`
	UserID    string `json:"userId"`
	Email     string `json:"canonicalEmail"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type ActionRequest struct {
	RequestID      string `json:"requestId"`
	UserID         string `json:"userId"`
	Email          string `json:"canonicalEmail"`
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type AccountCleanupRequest struct {
	UserID         string `json:"userId"`
	Email          string `json:"canonicalEmail"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type Exception struct {
	Code  string `json:"code"`
	Count int64  `json:"count,omitempty"`
}

type ExportRecord struct {
	RecordType     string    `json:"recordType"`
	RecordKey      string    `json:"recordKey"`
	TemplateID     string    `json:"templateId"`
	Channel        string    `json:"channel"`
	Status         string    `json:"status"`
	DeliveryStatus string    `json:"deliveryStatus,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ExportPage struct {
	Records     []ExportRecord `json:"records"`
	NextCursor  string         `json:"nextCursor,omitempty"`
	RecordCount int            `json:"recordCount"`
	Exceptions  []Exception    `json:"exceptions"`
}

type ActionResult struct {
	Owner          string   `json:"owner"`
	Action         string   `json:"action"`
	Status         string   `json:"status"`
	RecordCount    int64    `json:"recordCount"`
	RemainingCount int64    `json:"remainingCount"`
	ReasonCodes    []string `json:"reasonCodes"`
}

type AccountCleanupResult struct {
	Owner          string   `json:"owner"`
	Status         string   `json:"status"`
	RecordCount    int64    `json:"recordCount"`
	RemainingCount int64    `json:"remainingCount"`
	ReasonCodes    []string `json:"reasonCodes"`
}

func (s *Service) Export(ctx context.Context, request ExportRequest) (ExportPage, error) {
	if !validRequest(request.RequestID, request.UserID, request.Email) {
		return ExportPage{}, ErrInvalidRequest
	}
	limit := max(1, min(request.Limit, 100))
	after, err := decodeCursor(request.Cursor)
	if err != nil {
		return ExportPage{}, ErrInvalidRequest
	}
	keyIDs, hashes := candidateHashes(s.hashKeys, request.Email)
	if len(keyIDs) == 0 {
		return ExportPage{}, ErrInvalidRequest
	}
	rows, err := s.db.QueryContext(ctx, exportQuery, keyIDs, hashes, after.CreatedAt, after.ID, limit+1)
	if err != nil {
		return ExportPage{}, err
	}
	defer rows.Close()
	page := ExportPage{Records: make([]ExportRecord, 0, limit), Exceptions: []Exception{}}
	for rows.Next() {
		var record ExportRecord
		if err := rows.Scan(&record.RecordKey, &record.TemplateID, &record.Channel, &record.Status, &record.CreatedAt, &record.UpdatedAt, &record.DeliveryStatus); err != nil {
			return ExportPage{}, err
		}
		record.RecordType = "notification_message"
		if len(page.Records) == limit {
			last := page.Records[len(page.Records)-1]
			page.NextCursor = encodeCursor(cursor{CreatedAt: last.CreatedAt, ID: last.RecordKey})
			break
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return ExportPage{}, err
	}
	page.RecordCount = len(page.Records)
	return page, nil
}

func (s *Service) Apply(ctx context.Context, request ActionRequest) (ActionResult, error) {
	if !validRequest(request.RequestID, request.UserID, request.Email) || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 200 {
		return ActionResult{}, ErrInvalidRequest
	}
	switch request.Action {
	case "restrict_processing":
		return ActionResult{Owner: owner, Action: request.Action, Status: "not_applicable", ReasonCodes: []string{"ordinary_receipt_retention"}}, nil
	case "erase":
		result, err := s.erase(ctx, request.UserID, request.Email)
		if err != nil {
			return ActionResult{}, err
		}
		return ActionResult{Owner: owner, Action: request.Action, Status: result.Status, RecordCount: result.RecordCount, RemainingCount: result.RemainingCount, ReasonCodes: result.ReasonCodes}, nil
	default:
		return ActionResult{}, ErrInvalidRequest
	}
}

func (s *Service) CleanupAccount(ctx context.Context, request AccountCleanupRequest) (AccountCleanupResult, error) {
	if _, err := uuid.Parse(request.UserID); err != nil || !ValidEmail(request.Email) || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 200 {
		return AccountCleanupResult{}, ErrInvalidRequest
	}
	result, err := s.eraseIdempotent(ctx, request.UserID, request.Email, request.IdempotencyKey)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	return result, nil
}

func (s *Service) erase(ctx context.Context, userID, email string) (AccountCleanupResult, error) {
	return s.eraseIdempotent(ctx, userID, email, "")
}

func (s *Service) eraseIdempotent(ctx context.Context, userID, email, idempotencyKey string) (AccountCleanupResult, error) {
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
		subjectHash := sha256.Sum256([]byte("notification-account-cleanup:" + userID + ":" + strings.ToLower(strings.TrimSpace(email))))
		subjectRef := hex.EncodeToString(subjectHash[:])
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
	result, err := tx.ExecContext(ctx, eraseQuery, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes)
	if err != nil {
		return AccountCleanupResult{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AccountCleanupResult{}, err
	}
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
		   OR EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash)
		ORDER BY delivery.id
		FOR UPDATE OF delivery, message`, subjectKeyIDs, subjectHashes, emailKeyIDs, emailHashes)
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
	return nil
}

func validRequest(requestID, userID, email string) bool {
	_, requestErr := uuid.Parse(requestID)
	_, userErr := uuid.Parse(userID)
	return requestErr == nil && userErr == nil && ValidEmail(email)
}

func ValidEmail(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(normalized)
	return err == nil && parsed.Address == normalized
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

type cursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func encodeCursor(value cursor) string {
	encoded, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeCursor(value string) (cursor, error) {
	if value == "" {
		return cursor{CreatedAt: time.Time{}, ID: "00000000-0000-0000-0000-000000000000"}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, err
	}
	var result cursor
	if err := json.Unmarshal(decoded, &result); err != nil || result.CreatedAt.IsZero() {
		return cursor{}, errors.New("invalid cursor")
	}
	if _, err := uuid.Parse(result.ID); err != nil {
		return cursor{}, err
	}
	return result, nil
}

const exportQuery = `
	WITH candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, target_hash)
	)
	SELECT message.id::text, message.template_id, message.channel, message.status,
	       message.created_at, message.updated_at, COALESCE(delivery.status, '')
	FROM notification_messages AS message
	JOIN candidates ON candidates.hash_key_id=message.hash_key_id AND candidates.target_hash=message.target_hash
	LEFT JOIN LATERAL (
		SELECT status FROM notification_deliveries WHERE message_id=message.id ORDER BY created_at,id LIMIT 1
	) AS delivery ON true
	WHERE message.target_hash<>repeat('0',64) AND (message.created_at,message.id) > ($3::timestamptz,$4::uuid)
	ORDER BY message.created_at,message.id
	LIMIT $5`

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
	    OR EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash)
	  )`

const suppressOutboxQuery = `
	WITH subject_candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, subject_hash)
	), email_candidates AS (
		SELECT * FROM unnest($3::text[], $4::text[]) AS candidate(hash_key_id, target_hash)
	), messages AS (
		SELECT message.id FROM notification_messages message
		WHERE EXISTS (SELECT 1 FROM subject_candidates candidate WHERE candidate.hash_key_id=message.subject_hash_key_id AND candidate.subject_hash=message.subject_hash)
		   OR EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash)
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
		   OR EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash)
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
	   OR EXISTS (SELECT 1 FROM email_candidates candidate WHERE candidate.hash_key_id=message.hash_key_id AND candidate.target_hash=message.target_hash)`
