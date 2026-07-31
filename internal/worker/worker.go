package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
	"github.com/google/uuid"
)

const (
	leaseDuration  = 60 * time.Second
	sendTimeout    = 45 * time.Second
	releaseTimeout = 5 * time.Second
	maxAttempts    = 5

	statusQueued       = string(contracts.DeliveryStatusQueued)
	statusSending      = string(contracts.DeliveryStatusSending)
	statusSent         = string(contracts.DeliveryStatusSent)
	statusFailed       = string(contracts.DeliveryStatusFailed)
	statusDeadLettered = string(contracts.DeliveryStatusDeadLettered)
)

var ErrLeaseLost = errors.New("delivery lease lost")

type claim struct {
	DeliveryID        string
	MessageID         string
	TemplateID        string
	TemplateVersion   int
	Channel           string
	Attempt           int
	EncryptionKeyID   string
	TargetCiphertext  []byte
	PayloadCiphertext []byte
}

type claimResult struct {
	Status  string
	Claimed bool
	Claim   claim
}

type repository interface {
	claim(context.Context, string, time.Duration) (claimResult, error)
	markSent(context.Context, claim, providers.ProviderReceipt) error
	markRetry(context.Context, claim, string, time.Duration, string) error
	markFailed(context.Context, claim, string) error
	markDeadLettered(context.Context, claim, string) error
	release(context.Context, claim) error
}

type postgresStore struct {
	db *sql.DB
}

type Worker struct {
	repository      repository
	provider        providers.Provider
	keys            map[string][]byte
	retryDelay      func(int) time.Duration
	newID           func() string
	resolveTemplate func(string, int, string) (templates.Definition, error)
}

func New(db *sql.DB, provider providers.Provider, encryptionKey []byte) *Worker {
	return newWorker(postgresStore{db: db}, provider, encryptionKey)
}

func NewWithKeyring(db *sql.DB, provider providers.Provider, keys map[string][]byte) *Worker {
	return newWorkerWithKeyring(postgresStore{db: db}, provider, keys)
}

func newWorker(repository repository, provider providers.Provider, encryptionKey []byte) *Worker {
	return newWorkerWithKeyring(
		repository,
		provider,
		map[string][]byte{"legacy-v1": encryptionKey},
	)
}

func newWorkerWithKeyring(
	repository repository,
	provider providers.Provider,
	keys map[string][]byte,
) *Worker {
	return &Worker{
		repository:      repository,
		provider:        provider,
		keys:            keys,
		retryDelay:      retryDelay,
		newID:           uuid.NewString,
		resolveTemplate: templates.ResolveVersion,
	}
}

func (w *Worker) Process(ctx context.Context, message queue.BrokerMessage) error {
	result, err := w.repository.claim(ctx, message.DeliveryID(), leaseDuration)
	if err != nil {
		return fmt.Errorf("claim delivery: %w", err)
	}
	if !result.Claimed {
		if result.Status == statusFailed || result.Status == statusDeadLettered || result.Status == "" {
			return message.DeadLetter(ctx, terminalReason(result.Status))
		}
		return message.Complete(ctx)
	}

	claimed := result.Claim
	leaseHeld := true
	defer func() {
		if leaseHeld {
			releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			_ = w.repository.release(releaseCtx, claimed)
		}
	}()

	payload, err := w.render(claimed)
	if err != nil {
		if errors.Is(err, notificationcrypto.ErrKeyNotConfigured) {
			return fmt.Errorf("render delivery: %w", err)
		}
		if transitionErr := w.repository.markFailed(ctx, claimed, "invalid_payload"); transitionErr != nil {
			return errors.Join(fmt.Errorf("render delivery: %w", err), transitionErr)
		}
		leaseHeld = false
		return message.DeadLetter(ctx, "invalid_payload")
	}

	providerCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	receipt, providerErr := w.provider.Send(providerCtx, payload)
	cancel()
	if providerErr == nil {
		finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer finish()
		if err := w.repository.markSent(finishCtx, claimed, receipt); err != nil {
			return fmt.Errorf("mark delivery sent: %w", err)
		}
		leaseHeld = false
		return message.Complete(finishCtx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	kind, retryable, retryAfter := classifyProviderError(providerErr)
	if retryable && claimed.Attempt < maxAttempts {
		if retryAfter <= 0 {
			retryAfter = w.retryDelay(claimed.Attempt)
		}
		if err := w.repository.markRetry(
			ctx,
			claimed,
			string(kind),
			retryAfter,
			w.newID(),
		); err != nil {
			return fmt.Errorf("schedule delivery retry: %w", err)
		}
		leaseHeld = false
		return message.Complete(ctx)
	}

	if retryable {
		err = w.repository.markDeadLettered(ctx, claimed, string(kind))
	} else {
		err = w.repository.markFailed(ctx, claimed, string(kind))
	}
	if err != nil {
		return fmt.Errorf("mark delivery terminal: %w", err)
	}
	leaseHeld = false
	return message.DeadLetter(ctx, string(kind))
}

func (w *Worker) render(claimed claim) (providers.DeliveryPayload, error) {
	target, err := notificationcrypto.DecryptWithKeyID(
		w.keys,
		claimed.EncryptionKeyID,
		[]byte(claimed.MessageID+":target"),
		claimed.TargetCiphertext,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	payload, err := notificationcrypto.DecryptWithKeyID(
		w.keys,
		claimed.EncryptionKeyID,
		[]byte(claimed.MessageID+":payload"),
		claimed.PayloadCiphertext,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	var envelope struct {
		Locale string            `json:"locale"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return providers.DeliveryPayload{}, err
	}
	definition, err := w.resolveTemplate(
		claimed.TemplateID,
		claimed.TemplateVersion,
		claimed.Channel,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	email, err := templates.RenderEmail(
		definition,
		envelope.Locale,
		string(target),
		envelope.Fields,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	return providers.DeliveryPayload{
		Recipient: email.To,
		Subject:   email.Subject,
		Body:      email.Body,
		MessageID: fmt.Sprintf("<%s@notification.alive.org.tw>", claimed.DeliveryID),
	}, nil
}

func classifyProviderError(err error) (providers.ErrorKind, bool, time.Duration) {
	var providerErr *providers.ProviderError
	if !errors.As(err, &providerErr) {
		return providers.ErrorTemporary, true, 0
	}
	switch providerErr.Kind {
	case providers.ErrorTemporary, providers.ErrorRateLimited, providers.ErrorAcceptanceUnknown:
		return providerErr.Kind, true, providerErr.RetryAfter
	case providers.ErrorPermanent, providers.ErrorInvalidEndpoint, providers.ErrorSuppressed:
		return providerErr.Kind, false, 0
	default:
		return providers.ErrorPermanent, false, 0
	}
}

func retryDelay(attempt int) time.Duration {
	delay := time.Minute
	for current := 1; current < attempt && delay < time.Hour; current++ {
		delay *= 2
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func terminalReason(status string) string {
	if status == "" {
		return "delivery_not_found"
	}
	return status
}

func (s postgresStore) claim(
	ctx context.Context,
	deliveryID string,
	lease time.Duration,
) (claimResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claimResult{}, err
	}
	defer tx.Rollback()

	expired, err := expireDelivery(ctx, tx, deliveryID)
	if err != nil {
		return claimResult{}, err
	}
	if expired {
		if err := tx.Commit(); err != nil {
			return claimResult{}, err
		}
		return claimResult{Status: statusDeadLettered}, nil
	}

	var claimed claim
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
			          delivery.channel, delivery.attempt_count,
			          message.encryption_key_id, message.target_ciphertext, message.payload_ciphertext`,
		deliveryID,
		lease.Seconds(),
	).Scan(
		&claimed.DeliveryID,
		&claimed.MessageID,
		&claimed.TemplateID,
		&claimed.TemplateVersion,
		&claimed.Channel,
		&claimed.Attempt,
		&claimed.EncryptionKeyID,
		&claimed.TargetCiphertext,
		&claimed.PayloadCiphertext,
	)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='sending',
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.MessageID,
		); err != nil {
			return claimResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return claimResult{}, err
		}
		return claimResult{Status: statusSending, Claimed: true, Claim: claimed}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return claimResult{}, err
	}
	if err := tx.Rollback(); err != nil {
		return claimResult{}, err
	}

	var status string
	err = s.db.QueryRowContext(ctx, `
		SELECT status
		FROM notification_deliveries
		WHERE id=$1`,
		deliveryID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return claimResult{}, nil
	}
	if err != nil {
		return claimResult{}, err
	}
	return claimResult{Status: status}, nil
}

func expireDelivery(ctx context.Context, tx *sql.Tx, deliveryID string) (bool, error) {
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
	return s.transition(ctx, func(tx *sql.Tx) error {
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
	return s.transition(ctx, func(tx *sql.Tx) error {
		var nextAttempt time.Time
		err := tx.QueryRowContext(ctx, `
			UPDATE notification_deliveries
			SET status='queued',
			    next_attempt_at=clock_timestamp()+($3::bigint*interval '1 microsecond'),
			    last_error_code=$4,
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

func (s postgresStore) markTerminal(
	ctx context.Context,
	claimed claim,
	status string,
	code string,
) error {
	return s.transition(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE notification_deliveries
			SET status=$3,
			    last_error_code=$4,
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
	return s.transition(ctx, func(tx *sql.Tx) error {
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
	operation func(*sql.Tx) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
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
