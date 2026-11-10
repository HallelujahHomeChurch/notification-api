package graphfeedback

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/google/uuid"
)

var (
	ErrCursorMoved      = errors.New("Graph feedback checkpoint moved")
	ErrConflictingEvent = errors.New("conflicting Graph feedback event")
	ErrDeliveryBusy     = errors.New("Graph feedback delivery fence held")
	ErrReceiptPending   = errors.New("Graph feedback waits for an in-progress acceptance record")
)

type Cursor struct {
	Value      string
	ciphertext []byte
}

type Page struct {
	NextCursor string
	Events     []VerifiedEvent
}

type Result struct {
	Stored, Unmatched int
}

type Store struct {
	db          *sql.DB
	mailbox     uuid.UUID
	activeKeyID string
	keys        map[string][]byte
}

func NewStore(db *sql.DB, mailbox uuid.UUID, activeKeyID string, keys map[string][]byte) (*Store, error) {
	if db == nil || mailbox == uuid.Nil || activeKeyID == "" || len(keys[activeKeyID]) != 32 {
		return nil, errors.New("Graph feedback requires a mailbox and active cursor key")
	}
	return &Store{db: db, mailbox: mailbox, activeKeyID: activeKeyID, keys: keys}, nil
}

func (s *Store) cursorContext() []byte {
	return []byte("notification-graph-feedback:" + s.mailbox.String())
}

func (s *Store) Cursor(ctx context.Context) (Cursor, error) {
	var cursor Cursor
	var keyID string
	err := s.db.QueryRowContext(ctx, `
		SELECT cursor_ciphertext,COALESCE(encryption_key_id,'')
		FROM notification_email_feedback_cursors WHERE source='graph' AND mailbox_id=$1`, s.mailbox).
		Scan(&cursor.ciphertext, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, nil
	}
	if err != nil {
		return Cursor{}, err
	}
	value, err := notificationcrypto.DecryptWithKeyID(s.keys, keyID, s.cursorContext(), cursor.ciphertext)
	if err != nil {
		return Cursor{}, err
	}
	cursor.Value = string(value)
	return cursor, nil
}

func (s *Store) ApplyPage(ctx context.Context, previous Cursor, page Page) (Result, error) {
	if page.NextCursor == "" || len(page.NextCursor) > 8192 || len(page.Events) > 100 {
		return Result{}, errors.New("invalid Graph feedback page bounds")
	}
	ciphertext, err := notificationcrypto.EncryptWithKeyID(s.keys, s.activeKeyID, s.cursorContext(), []byte(page.NextCursor))
	if err != nil {
		return Result{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, string(s.cursorContext())).Scan(&locked)
	if err != nil {
		return Result{}, err
	}
	if !locked {
		return Result{}, ErrCursorMoved
	}
	var current []byte
	err = tx.QueryRowContext(ctx, `
		SELECT cursor_ciphertext FROM notification_email_feedback_cursors WHERE source='graph' AND mailbox_id=$1`, s.mailbox).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if !bytes.Equal(current, previous.ciphertext) {
		return Result{}, ErrCursorMoved
	}
	var result Result
	for _, event := range page.Events {
		matched, stored, err := s.applyEvent(ctx, tx, event)
		if err != nil {
			return Result{}, err
		}
		if !matched {
			result.Unmatched++
		}
		if stored {
			result.Stored++
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO notification_email_feedback_cursors(source,mailbox_id,cursor_ciphertext,encryption_key_id)
		VALUES('graph',$1,$2,$3) ON CONFLICT(source,mailbox_id)
		DO UPDATE SET cursor_ciphertext=EXCLUDED.cursor_ciphertext,encryption_key_id=EXCLUDED.encryption_key_id,updated_at=clock_timestamp()`,
		s.mailbox, ciphertext, s.activeKeyID); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Store) applyEvent(ctx context.Context, tx *sql.Tx, event VerifiedEvent) (matched, stored bool, err error) {
	if !event.validFor(s.mailbox) {
		return false, false, nil
	}
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+event.DeliveryID.String()).Scan(&locked)
	if err != nil {
		return false, false, err
	}
	if !locked {
		return false, false, ErrDeliveryBusy
	}
	var outcome string
	var fresh bool
	const receiptSettleWindow = 2 * time.Minute
	err = tx.QueryRowContext(ctx, `
		SELECT a.outcome,a.started_at>clock_timestamp()-($6::bigint*interval '1 microsecond')
		FROM notification_deliveries d
		JOIN notification_messages m ON m.id=d.message_id
		JOIN notification_email_attempts a ON a.delivery_id=d.id
		WHERE d.id=$1 AND d.channel='email' AND d.provider='graph' AND d.graph_mailbox_id=$5
		  AND a.provider='graph' AND a.attempt=$2
		  AND m.target_hash<>repeat('0',64)
		  AND ((m.hash_key_id=$3 AND m.target_hash=$4) OR m.target_hashes->>$3=$4)
		`, event.DeliveryID, event.Attempt, event.RecipientHashKeyID, event.RecipientHash, s.mailbox, receiptSettleWindow.Microseconds()).Scan(&outcome, &fresh)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if outcome != "accepted" {
		if fresh && (outcome == "started" || outcome == "unknown") {
			return false, false, ErrReceiptPending
		}
		return false, false, nil
	}
	key, at := event.key(), event.OccurredAt.UTC().Truncate(time.Microsecond)
	insert, err := tx.ExecContext(ctx, `
		INSERT INTO notification_email_delivery_events(source,event_key,delivery_id,outcome,event_at)
		VALUES('graph',$1,$2,$3,$4) ON CONFLICT(source,event_key) DO NOTHING`, key, event.DeliveryID, event.Outcome, at)
	if err != nil {
		return false, false, err
	}
	n, err := insert.RowsAffected()
	if err != nil || n != 0 {
		return true, n == 1, err
	}
	var same bool
	err = tx.QueryRowContext(ctx, `
		SELECT delivery_id=$2 AND outcome=$3 AND event_at=$4
		FROM notification_email_delivery_events WHERE source='graph' AND event_key=$1`,
		key, event.DeliveryID, event.Outcome, at).Scan(&same)
	if err != nil {
		return false, false, err
	}
	if !same {
		return false, false, ErrConflictingEvent
	}
	return true, false, nil
}
