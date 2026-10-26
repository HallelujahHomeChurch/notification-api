package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

type postgresStore struct {
	db   *sql.DB
	conn *sql.Conn
}

// A claimed delivery owns this connection until its cancellation fence releases.
// Binding copies per-execution collaborators; concurrent Process calls never
// mutate the shared Worker or email policy.
type deliverySession struct {
	db   *sql.DB
	conn *sql.Conn
}

func (s *deliverySession) bind(w *Worker) *Worker {
	scoped := *w
	scoped.repository = postgresStore{db: s.db, conn: s.conn}
	if w.email != nil {
		policy := *w.email
		policy.conn = s.conn
		scoped.email = &policy
	}
	return &scoped
}

// Email limits and delivery state share one database in every composition root.
// Reject a foreign pool before claiming; never silently run its policy on ours.
func (w *Worker) validateEmailDatabase() error {
	if store, ok := w.repository.(postgresStore); ok && w.email != nil && w.email.db != store.db {
		return errors.New("email policy must use the delivery database")
	}
	return nil
}

type deliveryStatements interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type deliveryTransaction interface {
	deliveryStatements
	Commit() error
	Rollback() error
}

// SessionTransaction deliberately avoids sql.Tx's cancellation goroutine racing
// advisory unlock. Unfenced callers retain their existing sql.DB transaction.
func beginDeliveryTransaction(ctx context.Context, db *sql.DB, conn *sql.Conn) (deliveryTransaction, error) {
	if conn != nil {
		return database.BeginSessionTransaction(ctx, conn)
	}
	return db.BeginTx(ctx, nil)
}

func (s postgresStore) claim(
	ctx context.Context,
	deliveryID string,
	lease time.Duration,
) (claimResult, error) {
	result, release, err := s.claimFenced(ctx, deliveryID, lease)
	if err != nil {
		return claimResult{}, err
	}
	if release != nil {
		if err := release(); err != nil {
			return claimResult{}, err
		}
	}
	result.session = nil // claim() releases its session before returning.
	return result, nil
}

func (s postgresStore) claimFenced(
	ctx context.Context,
	deliveryID string,
	lease time.Duration,
) (claimResult, func() error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return claimResult{}, nil, err
	}
	lockKey := "notification-dsr-delivery:" + deliveryID
	release := func() error {
		releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		_, unlockErr := conn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
		if unlockErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		return errors.Join(unlockErr, conn.Close())
	}
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return claimResult{}, nil, err
	}
	if !locked {
		_ = conn.Close()
		return claimResult{}, nil, errors.New("delivery fence held")
	}
	tx, err := database.BeginSessionTransaction(ctx, conn)
	if err != nil {
		_ = release()
		return claimResult{}, nil, err
	}

	recovered, err := restoreAcceptedEmail(ctx, tx, deliveryID)
	if err != nil {
		_ = tx.Rollback()
		_ = release()
		return claimResult{}, nil, err
	}
	if recovered {
		if err := tx.Commit(); err != nil {
			_ = release()
			return claimResult{}, nil, err
		}
		_ = release()
		return claimResult{Status: statusSent}, nil, nil
	}
	expired, err := expireDelivery(ctx, tx, deliveryID)
	if err != nil {
		_ = tx.Rollback()
		_ = release()
		return claimResult{}, nil, err
	}
	if expired {
		if err := tx.Commit(); err != nil {
			_ = release()
			return claimResult{}, nil, err
		}
		_ = release()
		return claimResult{Status: statusDeadLettered}, nil, nil
	}

	var claimed claim
	var callerID, idempotencyKey string
	err = tx.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT delivery.id
			FROM notification_deliveries delivery
			WHERE delivery.id=$1
			  AND (
			      (delivery.status='queued' AND delivery.next_attempt_at <= clock_timestamp())
			      OR
			      (delivery.status='sending'
			       AND COALESCE(delivery.lease_expires_at, '-infinity'::timestamptz) <= clock_timestamp())
			  )
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notification_deliveries AS delivery
		SET status='sending',
		    attempt_count=delivery.attempt_count+1,
		    lease_expires_at=clock_timestamp()+($2::double precision*interval '1 second'),
		    updated_at=clock_timestamp()
		FROM candidate, notification_messages AS message
		WHERE delivery.id=candidate.id AND message.id=delivery.message_id
			RETURNING delivery.id, message.id, message.template_id, message.template_version,
			          delivery.channel, delivery.attempt_count, delivery.failure_count, message.expires_at,
			          message.encryption_key_id, message.target_ciphertext, message.payload_ciphertext,
			          COALESCE(message.eligibility_campaign_id::text,''), COALESCE(message.eligibility_recipient_id::text,''), message.caller_app_id, message.idempotency_key`,
		deliveryID,
		lease.Seconds(),
	).Scan(
		&claimed.DeliveryID,
		&claimed.MessageID,
		&claimed.TemplateID,
		&claimed.TemplateVersion,
		&claimed.Channel,
		&claimed.Attempt,
		&claimed.FailureCount,
		&claimed.ExpiresAt,
		&claimed.EncryptionKeyID,
		&claimed.TargetCiphertext,
		&claimed.PayloadCiphertext,
		&claimed.EligibilityCampaignID,
		&claimed.EligibilityRecipientID,
		&callerID, &idempotencyKey,
	)
	if err == nil {
		claimed.EligibilityRef = legacyCampaignEligibility(callerID, idempotencyKey)
		if claimed.EligibilityCampaignID != "" {
			claimed.EligibilityRef = &contracts.EligibilityRef{CampaignID: claimed.EligibilityCampaignID, RecipientID: claimed.EligibilityRecipientID}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='sending',
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
		); err != nil {
			_ = tx.Rollback()
			_ = release()
			return claimResult{}, nil, err
		}
		if err := tx.Commit(); err != nil {
			_ = release()
			return claimResult{}, nil, err
		}
		return claimResult{Status: statusSending, Claimed: true, Claim: claimed, session: &deliverySession{db: s.db, conn: conn}}, release, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		_ = release()
		return claimResult{}, nil, err
	}
	if err := tx.Rollback(); err != nil {
		_ = release()
		return claimResult{}, nil, err
	}

	var status string
	err = conn.QueryRowContext(ctx, `
		SELECT status
		FROM notification_deliveries
		WHERE id=$1`,
		deliveryID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		_ = release()
		return claimResult{}, nil, nil
	}
	if err != nil {
		_ = release()
		return claimResult{}, nil, err
	}
	_ = release()
	return claimResult{Status: status}, nil, nil
}

func expireDelivery(ctx context.Context, tx *database.SessionTransaction, deliveryID string) (bool, error) {
	outboxRows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM notification_outbox
		WHERE delivery_id=$1 AND status<>'published'
		ORDER BY id
		FOR UPDATE`,
		deliveryID,
	)
	if err != nil {
		return false, err
	}
	for outboxRows.Next() {
		var outboxID string
		if err := outboxRows.Scan(&outboxID); err != nil {
			outboxRows.Close()
			return false, err
		}
	}
	if err := outboxRows.Close(); err != nil {
		return false, err
	}

	var messageID string
	var expired bool
	err = tx.QueryRowContext(ctx, `
		SELECT message.id, message.expires_at <= clock_timestamp()
		FROM notification_deliveries AS delivery
		JOIN notification_messages AS message ON message.id=delivery.message_id
		WHERE delivery.id=$1
		  AND (
		        delivery.status='queued'
		        OR (
		            delivery.status='sending'
		            AND COALESCE(delivery.lease_expires_at, '-infinity'::timestamptz)
		                <= clock_timestamp()
		        )
		      )
		  AND message.status IN ('queued','sending')
		FOR UPDATE OF delivery, message`,
		deliveryID,
	).Scan(&messageID, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !expired {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_deliveries
		SET status='dead_lettered',
		    last_error_code='expired',
		    lease_expires_at=NULL,
		    updated_at=clock_timestamp()
		WHERE id=$1`,
		deliveryID,
	); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_messages
		SET status='dead_lettered',
		    terminal_at=clock_timestamp(),
		    updated_at=clock_timestamp()
		WHERE id=$1`,
		messageID,
	); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM notification_outbox
		WHERE delivery_id=$1 AND status<>'published'`,
		deliveryID,
	); err != nil {
		return false, err
	}
	return true, nil
}

func (s postgresStore) markSent(
	ctx context.Context,
	claimed claim,
	receipt providers.ProviderReceipt,
) error {
	return s.transition(ctx, func(tx deliveryTransaction) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE notification_deliveries
			SET status='sent',
			    sent_at=clock_timestamp(),
			    provider_message_id=NULLIF($3,''),
			    last_error_code=NULL,
			    lease_expires_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1 AND status='sending' AND attempt_count=$2`,
			claimed.DeliveryID,
			claimed.Attempt,
			receipt.ProviderMessageID,
		)
		if err := transitionResult(result, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='sent',
			    terminal_at=clock_timestamp(),
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
		)
		return err
	})
}

func (s postgresStore) markRetry(
	ctx context.Context,
	claimed claim,
	code string,
	delay time.Duration,
	outboxID string,
) error {
	return s.transition(ctx, func(tx deliveryTransaction) error {
		var nextAttempt time.Time
		err := tx.QueryRowContext(ctx, `
			UPDATE notification_deliveries
			SET status='queued',
			    next_attempt_at=clock_timestamp()+($3::bigint*interval '1 microsecond'),
			    last_error_code=$4,
       failure_count=failure_count+CASE WHEN channel='email' AND $4='rate_limited' THEN 0 ELSE 1 END,
       lease_expires_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1 AND status='sending' AND attempt_count=$2
			RETURNING next_attempt_at`,
			claimed.DeliveryID,
			claimed.Attempt,
			delay.Microseconds(),
			code,
		).Scan(&nextAttempt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='queued',
			    terminal_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
		); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (
				id,delivery_id,status,next_attempt_at
			) VALUES ($1,$2,'pending',$3)`,
			outboxID,
			claimed.DeliveryID,
			nextAttempt,
		)
		return err
	})
}

func (s postgresStore) markFailed(ctx context.Context, claimed claim, code string) error {
	return s.markTerminal(ctx, claimed, statusFailed, code)
}

func (s postgresStore) markDeadLettered(ctx context.Context, claimed claim, code string) error {
	return s.markTerminal(ctx, claimed, statusDeadLettered, code)
}

func (s postgresStore) markSuppressed(ctx context.Context, claimed claim, code string) error {
	return s.markTerminal(ctx, claimed, statusSuppressed, code)
}

func (s postgresStore) markTerminal(
	ctx context.Context,
	claimed claim,
	status string,
	code string,
) error {
	return s.transition(ctx, func(tx deliveryTransaction) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE notification_deliveries
			SET status=$3,
			    last_error_code=$4,
       failure_count=failure_count+CASE WHEN channel='email' AND $4='rate_limited' THEN 0 ELSE 1 END,
       lease_expires_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1 AND status='sending' AND attempt_count=$2`,
			claimed.DeliveryID,
			claimed.Attempt,
			status,
			code,
		)
		if err := transitionResult(result, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status=$2,
			    terminal_at=clock_timestamp(),
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
			status,
		)
		return err
	})
}

func (s postgresStore) release(ctx context.Context, claimed claim) error {
	return s.transition(ctx, func(tx deliveryTransaction) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE notification_deliveries
			SET status='queued',
			    next_attempt_at=clock_timestamp(),
			    lease_expires_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1 AND status='sending' AND attempt_count=$2`,
			claimed.DeliveryID,
			claimed.Attempt,
		)
		if err := transitionResult(result, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='queued',
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
		)
		return err
	})
}

func (s postgresStore) transition(
	ctx context.Context,
	operation func(deliveryTransaction) error,
) error {
	tx, err := beginDeliveryTransaction(ctx, s.db, s.conn)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := operation(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func transitionResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseLost
	}
	return nil
}
