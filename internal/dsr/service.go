package dsr

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	Owner       string   `json:"owner"`
	Action      string   `json:"action"`
	Status      string   `json:"status"`
	RecordCount int64    `json:"recordCount"`
	ReasonCodes []string `json:"reasonCodes"`
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
		keyIDs, hashes := candidateHashes(s.hashKeys, request.Email)
		if len(keyIDs) == 0 {
			return ActionResult{}, ErrInvalidRequest
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return ActionResult{}, err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, eraseQuery, keyIDs, hashes)
		if err != nil {
			return ActionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ActionResult{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return ActionResult{}, err
		}
		return ActionResult{Owner: owner, Action: request.Action, Status: "completed", RecordCount: count, ReasonCodes: []string{}}, nil
	default:
		return ActionResult{}, ErrInvalidRequest
	}
}

func validRequest(requestID, userID, email string) bool {
	_, requestErr := uuid.Parse(requestID)
	_, userErr := uuid.Parse(userID)
	return requestErr == nil && userErr == nil && strings.TrimSpace(email) != ""
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
	WITH candidates AS (
		SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, target_hash)
	)
	UPDATE notification_messages AS message
	SET target_ciphertext=''::bytea,
		payload_ciphertext=''::bytea,
		target_hash=repeat('0',64),
		payload_purged_at=COALESCE(payload_purged_at, clock_timestamp()),
		updated_at=clock_timestamp()
	FROM candidates
	WHERE message.hash_key_id=candidates.hash_key_id
	  AND message.target_hash=candidates.target_hash
	  AND message.target_hash<>repeat('0',64)`
