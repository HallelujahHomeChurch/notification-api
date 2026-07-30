package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
)

const (
	leaseDuration       = 60 * time.Second
	publishTimeout      = 45 * time.Second
	maxRetryDelay       = time.Minute
	idlePollInterval    = 250 * time.Millisecond
	defaultFailureLimit = 5
)

var (
	ErrLeaseLost  = errors.New("outbox lease lost")
	errClaim      = errors.New("outbox claim failed")
	errPublish    = errors.New("outbox publish failed")
	errTransition = errors.New("outbox transition failed")
)

type claim struct {
	OutboxID   string
	DeliveryID string
	Attempt    int
	Expired    bool
}

type store interface {
	claimNext(context.Context, time.Duration) (claim, bool, error)
	markPublished(context.Context, claim) error
	markRetry(context.Context, claim, time.Duration) error
}

type postgresStore struct {
	db *sql.DB
}

type Dispatcher struct {
	store                  store
	publisher              queue.Publisher
	random                 func(int64) int64
	wait                   func(context.Context, time.Duration) error
	publishTimeout         time.Duration
	maxConsecutiveFailures int
}

func New(db *sql.DB, publisher queue.Publisher) *Dispatcher {
	return newDispatcher(postgresStore{db: db}, publisher)
}

func newDispatcher(store store, publisher queue.Publisher) *Dispatcher {
	return &Dispatcher{
		store:                  store,
		publisher:              publisher,
		random:                 rand.Int64N,
		wait:                   wait,
		publishTimeout:         publishTimeout,
		maxConsecutiveFailures: defaultFailureLimit,
	}
}

func (d *Dispatcher) DispatchOne(ctx context.Context) (bool, error) {
	claimed, ok, err := d.store.claimNext(ctx, leaseDuration)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errClaim, err)
	}
	if !ok {
		return false, nil
	}
	if claimed.Expired {
		return true, nil
	}
	publishCtx, cancel := context.WithTimeout(ctx, d.publishTimeout)
	err = d.publisher.Publish(publishCtx, claimed.OutboxID, claimed.DeliveryID)
	cancel()
	if err != nil {
		retryErr := d.store.markRetry(ctx, claimed, d.retryDelay(claimed.Attempt))
		if retryErr != nil {
			retryErr = fmt.Errorf("%w: %w", errTransition, retryErr)
		}
		return true, errors.Join(fmt.Errorf("%w: %w", errPublish, err), retryErr)
	}
	if err := d.store.markPublished(ctx, claimed); err != nil {
		return true, fmt.Errorf("%w: %w", errTransition, err)
	}
	return true, nil
}

func (d *Dispatcher) Run(ctx context.Context) error {
	claimFailures := 0
	publishFailures := 0
	transitionFailures := 0
	for {
		processed, err := d.DispatchOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		if errors.Is(err, errClaim) {
			claimFailures++
		} else {
			claimFailures = 0
		}
		if errors.Is(err, errPublish) {
			publishFailures++
		} else if processed {
			publishFailures = 0
		}
		if errors.Is(err, errTransition) {
			transitionFailures++
		} else if processed {
			transitionFailures = 0
		}

		if err != nil {
			failures := max(claimFailures, publishFailures, transitionFailures)
			if failures >= d.maxConsecutiveFailures {
				return fmt.Errorf("persistent outbox dependency failure after %d attempts: %w", failures, err)
			}
			if err := d.wait(ctx, d.retryDelay(failures)); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := d.wait(ctx, idlePollInterval); err != nil {
			return err
		}
	}
}

func (d *Dispatcher) retryDelay(attempt int) time.Duration {
	delay := time.Second
	for current := 1; current < attempt && delay < maxRetryDelay; current++ {
		delay *= 2
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
	half := delay / 2
	return half + time.Duration(d.random(int64(delay-half)))
}

func (s postgresStore) claimNext(ctx context.Context, lease time.Duration) (claim, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claim{}, false, fmt.Errorf("begin notification outbox claim: %w", err)
	}
	defer tx.Rollback()

	var claimed claim
	var messageID string
	err = tx.QueryRowContext(ctx, `
		SELECT outbox.id, delivery.id, message.id,
		       message.expires_at <= clock_timestamp()
		FROM notification_outbox AS outbox
		JOIN notification_deliveries AS delivery ON delivery.id=outbox.delivery_id
		JOIN notification_messages AS message ON message.id=delivery.message_id
		WHERE (
		        (outbox.status='pending' AND outbox.next_attempt_at <= clock_timestamp())
		        OR
		        (outbox.status='publishing'
		         AND COALESCE(outbox.lease_expires_at, '-infinity'::timestamptz) <= clock_timestamp())
		      )
		  AND delivery.status='queued'
		  AND message.status='queued'
		ORDER BY COALESCE(outbox.lease_expires_at, outbox.next_attempt_at),
		         outbox.created_at, outbox.id
		FOR UPDATE OF outbox, delivery, message SKIP LOCKED
		LIMIT 1`,
	).Scan(&claimed.OutboxID, &claimed.DeliveryID, &messageID, &claimed.Expired)
	if errors.Is(err, sql.ErrNoRows) {
		return claim{}, false, nil
	}
	if err != nil {
		return claim{}, false, fmt.Errorf("select notification outbox: %w", err)
	}
	if claimed.Expired {
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_deliveries
			SET status='dead_lettered',
			    last_error_code='expired',
			    lease_expires_at=NULL,
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			claimed.DeliveryID,
		); err != nil {
			return claim{}, false, fmt.Errorf("expire notification delivery: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_messages
			SET status='dead_lettered',
			    terminal_at=clock_timestamp(),
			    updated_at=clock_timestamp()
			WHERE id=$1`,
			messageID,
		); err != nil {
			return claim{}, false, fmt.Errorf("expire notification message: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_outbox WHERE id=$1`, claimed.OutboxID); err != nil {
			return claim{}, false, fmt.Errorf("delete expired notification outbox: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return claim{}, false, fmt.Errorf("commit expired notification outbox: %w", err)
		}
		return claimed, true, nil
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE notification_outbox
		SET status='publishing',
		    attempt_count=attempt_count+1,
		    lease_expires_at=clock_timestamp()+($2::double precision*interval '1 second'),
		    updated_at=clock_timestamp()
		WHERE id=$1
		RETURNING attempt_count`,
		claimed.OutboxID,
		lease.Seconds(),
	).Scan(&claimed.Attempt)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim notification outbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return claim{}, false, fmt.Errorf("commit notification outbox claim: %w", err)
	}
	return claimed, true, nil
}

func (s postgresStore) markPublished(ctx context.Context, claimed claim) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_outbox
		SET status='published',
		    published_at=clock_timestamp(),
		    lease_expires_at=NULL,
		    updated_at=clock_timestamp()
		WHERE id=$1 AND status='publishing' AND attempt_count=$2`,
		claimed.OutboxID, claimed.Attempt,
	)
	return transitionResult(result, err, "mark notification outbox published")
}

func (s postgresStore) markRetry(ctx context.Context, claimed claim, delay time.Duration) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_outbox
		SET status='pending',
		    next_attempt_at=clock_timestamp()+($3::bigint*interval '1 microsecond'),
		    lease_expires_at=NULL,
		    updated_at=clock_timestamp()
		WHERE id=$1 AND status='publishing' AND attempt_count=$2`,
		claimed.OutboxID, claimed.Attempt, delay.Microseconds(),
	)
	return transitionResult(result, err, "reschedule notification outbox")
}

func transitionResult(result sql.Result, err error, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows: %w", operation, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s: %w", operation, ErrLeaseLost)
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
