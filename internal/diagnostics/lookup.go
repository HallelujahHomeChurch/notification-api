package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/dsr"
)

const lookupQuery = `
WITH candidates AS (
    SELECT * FROM unnest($1::text[], $2::text[]) AS candidate(hash_key_id, target_hash)
)
SELECT message.id, delivery.id, message.status, delivery.status,
       message.created_at, outbox.published_at, delivery.sent_at,
       delivery.attempt_count, COALESCE(delivery.last_error_code, ''),
       COALESCE(delivery.provider_message_id, ''),
       COALESCE(outbox_state.status, ''), COALESCE(outbox_state.attempt_count, 0)
FROM notification_messages AS message
JOIN candidates USING (hash_key_id, target_hash)
JOIN notification_deliveries AS delivery ON delivery.message_id = message.id
LEFT JOIN LATERAL (
    SELECT min(published_at) AS published_at
    FROM notification_outbox
    WHERE delivery_id = delivery.id
) AS outbox ON true
LEFT JOIN LATERAL (
    SELECT status, attempt_count
    FROM notification_outbox
    WHERE delivery_id = delivery.id
    ORDER BY created_at DESC, id DESC
    LIMIT 1
) AS outbox_state ON true
WHERE message.caller_app_id = 'account-api'
  AND message.template_id = 'account.oauth-onboarding-code'
  AND message.created_at >= $3 AND message.created_at < $4
ORDER BY message.created_at, message.id
LIMIT 100`

type Record struct {
	MessageID          string     `json:"messageId"`
	DeliveryID         string     `json:"deliveryId"`
	MessageStatus      string     `json:"messageStatus"`
	DeliveryStatus     string     `json:"deliveryStatus"`
	CreatedAt          time.Time  `json:"createdAt"`
	PublishedAt        *time.Time `json:"publishedAt,omitempty"`
	SentAt             *time.Time `json:"sentAt,omitempty"`
	AttemptCount       int        `json:"attemptCount"`
	LastErrorCode      string     `json:"lastErrorCode,omitempty"`
	ProviderMessageID  string     `json:"providerMessageId,omitempty"`
	OutboxStatus       string     `json:"outboxStatus,omitempty"`
	OutboxAttemptCount int        `json:"outboxAttemptCount"`
	QueueSeconds       *float64   `json:"queueSeconds,omitempty"`
	SMTPSeconds        *float64   `json:"smtpSeconds,omitempty"`
}

// Lookup reads a bounded delivery timeline without returning recipient or payload data.
func Lookup(ctx context.Context, db *sql.DB, hashKeys map[string][]byte, email string, from, to time.Time) ([]Record, error) {
	if !dsr.ValidEmail(email) || len(hashKeys) == 0 || from.IsZero() || to.IsZero() || !from.Before(to) || to.Sub(from) > 31*24*time.Hour {
		return nil, errors.New("email, hash keys, and a time range up to 31 days are required")
	}
	if db == nil {
		return nil, errors.New("database is required")
	}
	ids, hashes := candidateHashes(hashKeys, email)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, lookupQuery, ids, hashes, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]Record, 0)
	for rows.Next() {
		var record Record
		var published, sent sql.NullTime
		if err := rows.Scan(&record.MessageID, &record.DeliveryID, &record.MessageStatus, &record.DeliveryStatus,
			&record.CreatedAt, &published, &sent, &record.AttemptCount, &record.LastErrorCode,
			&record.ProviderMessageID, &record.OutboxStatus, &record.OutboxAttemptCount); err != nil {
			return nil, err
		}
		if published.Valid {
			record.PublishedAt = &published.Time
			seconds := published.Time.Sub(record.CreatedAt).Seconds()
			record.QueueSeconds = &seconds
		}
		if sent.Valid {
			record.SentAt = &sent.Time
			seconds := sent.Time.Sub(record.CreatedAt).Seconds()
			record.SMTPSeconds = &seconds
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func candidateHashes(keys map[string][]byte, email string) ([]string, []string) {
	email = strings.ToLower(strings.TrimSpace(email))
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	hashes := make([]string, 0, len(ids))
	for _, id := range ids {
		hashes = append(hashes, notificationcrypto.Hash(keys[id], []byte(email)))
	}
	return ids, hashes
}
