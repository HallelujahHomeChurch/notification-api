package sesfeedback

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

type Store struct{ DB *sql.DB }

func (s Store) Handle(ctx context.Context, e Event) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deliveryID, messageID, hashID, targetHash string
	// A tag can reconcile an unknown receipt only after an actual SES attempt was reserved.
	const resolveQuery = `SELECT d.id::text,d.message_id::text,m.hash_key_id,m.target_hash FROM notification_deliveries d JOIN notification_messages m ON m.id=d.message_id WHERE d.channel='email' AND EXISTS(SELECT 1 FROM notification_email_attempts a WHERE a.delivery_id=d.id AND a.provider='ses' AND (d.provider_message_id=$1 OR a.provider_message_id=$1 OR ($2::text<>'' AND d.id::text=$2 AND d.provider_message_id IS NULL AND a.provider_message_id IS NULL AND a.outcome IN ('started','unknown')))) AND ($2::text='' OR d.id::text=$2) LIMIT 1`
	err = tx.QueryRowContext(ctx, resolveQuery, e.MessageID, e.DeliveryID).Scan(&deliveryID, &messageID, &hashID, &targetHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnmatchedEvent
	}
	if err != nil {
		return err
	}
	var locked bool
	// Do not occupy a pool connection waiting for a sender that needs it to finish.
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+deliveryID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrDeliveryInFlight
	}
	// Serialize the suppression decision with the sender's admission gate, never its network call.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM notification_email_budget WHERE id=true FOR UPDATE`); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, resolveQuery, e.MessageID, e.DeliveryID).Scan(&deliveryID, &messageID, &hashID, &targetHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnmatchedEvent
		}
		return err
	}

	if targetHash == "0000000000000000000000000000000000000000000000000000000000000000" {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO notification_email_feedback(sns_message_id,delivery_id,provider_message_id,event_type,event_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, e.SNSMessageID, deliveryID, e.MessageID, e.Kind, e.At)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var same bool
		if err := tx.QueryRowContext(ctx, `SELECT delivery_id=$2::uuid AND provider_message_id=$3 AND event_type=$4 AND event_at=$5 FROM notification_email_feedback WHERE sns_message_id=$1`, e.SNSMessageID, deliveryID, e.MessageID, e.Kind, e.At).Scan(&same); err != nil {
			return err
		}
		if !same {
			return ErrInvalidEvent
		}
		return tx.Commit()
	}
	if e.Kind == "hard_bounce" || e.Kind == "complaint" {
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_email_suppressions(hash_key_id,target_hash,reason) SELECT candidate.key,candidate.value,$2 FROM notification_messages m CROSS JOIN LATERAL jsonb_each_text(m.target_hashes || jsonb_build_object(m.hash_key_id,m.target_hash)) candidate WHERE m.id=$1 ON CONFLICT(hash_key_id,target_hash) DO UPDATE SET reason=CASE WHEN notification_email_suppressions.reason='complaint' THEN 'complaint' ELSE EXCLUDED.reason END,updated_at=clock_timestamp()`, messageID, e.Kind)
		if err != nil {
			return err
		}
	}
	// A genuine provider event proves acceptance, including after a lost SendEmail response.
	if _, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET provider='ses',status='sent',provider_message_id=COALESCE(provider_message_id,$2),sent_at=COALESCE(sent_at,$3),last_error_code=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, deliveryID, e.MessageID, e.At); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_messages SET status='sent',terminal_at=COALESCE(terminal_at,clock_timestamp()),updated_at=clock_timestamp() WHERE id=$1`, messageID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if e.Kind == "hard_bounce" || e.Kind == "complaint" {
		slog.Warn("ses_feedback_suppression_applied", "reason", e.Kind)
	}
	return nil
}
