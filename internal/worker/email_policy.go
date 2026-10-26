package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

var errEmailAcceptanceUnknown = errors.New("email acceptance requires reconciliation")
var errEmailSuppressed = errors.New("email is technically suppressed")
var errEmailProviderMismatch = errors.New("email provider does not match pinned delivery")

var errEmailAlreadyAccepted = errors.New("email already accepted")

type EmailLimits struct{ PerMinute, PerHour, ReserveMinute, ReserveHour int }

func (l EmailLimits) Validate() error {
	if l.PerMinute <= 0 || l.PerHour <= 0 || l.ReserveMinute < 0 || l.ReserveHour < 0 || l.ReserveMinute >= l.PerMinute || l.ReserveHour >= l.PerHour {
		return errors.New("SMTP quotas must be positive and reserves smaller than quotas")
	}
	return nil
}

type emailPolicy struct {
	db       *sql.DB
	conn     *sql.Conn
	limits   EmailLimits
	provider string
}

func (p *emailPolicy) statements() deliveryStatements {
	if p.conn != nil {
		return p.conn
	}
	return p.db
}

func (w *Worker) WithEmailLimits(db *sql.DB, limits EmailLimits) *Worker {
	name := "smtp"
	if p, ok := w.providers["email"].(interface{ Family() string }); ok {
		name = p.Family()
	}
	w.email = &emailPolicy{db: db, limits: limits, provider: name}
	return w
}

func campaignEmail(c claim) bool { return strings.HasPrefix(c.TemplateID, "engagement.") }

func (p *emailPolicy) begin(ctx context.Context, c claim) (time.Duration, error) {
	if err := p.limits.Validate(); err != nil {
		return 0, err
	}
	tx, err := beginDeliveryTransaction(ctx, p.db, p.conn)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var now, next, cooldown time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp(),next_send_at,cooldown_until FROM notification_email_budget WHERE id=true FOR UPDATE`).Scan(&now, &next, &cooldown); err != nil {
		return 0, err
	}
	name := p.provider
	if name == "" {
		name = "smtp"
	}
	var pinned string
	if err = tx.QueryRowContext(ctx, `SELECT provider FROM notification_deliveries WHERE id=$1`, c.DeliveryID).Scan(&pinned); err != nil {
		return 0, err
	}
	if pinned != name {
		return 0, errEmailProviderMismatch
	}
	var suppressed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_messages m CROSS JOIN LATERAL jsonb_each_text(m.target_hashes || jsonb_build_object(m.hash_key_id,m.target_hash)) candidate JOIN notification_email_suppressions s ON s.hash_key_id=candidate.key AND s.target_hash=candidate.value WHERE m.id=$1)`, c.MessageID).Scan(&suppressed); err != nil {
		return 0, err
	}
	if suppressed {
		return 0, errEmailSuppressed
	}
	var uncertain, accepted bool
	var recorded int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE attempt<$2),COALESCE(bool_or(outcome IN ('started','unknown')),false),COALESCE(bool_or(outcome='accepted'),false) FROM notification_email_attempts WHERE delivery_id=$1`, c.DeliveryID, c.Attempt).Scan(&recorded, &uncertain, &accepted); err != nil {
		return 0, err
	}
	if accepted {
		return 0, errEmailAlreadyAccepted
	}
	// A previous writer or a crash left an unobserved claim; never invent its acceptance result.
	if recorded != c.Attempt-1 {
		if _, err = tx.ExecContext(ctx, `UPDATE notification_messages SET email_history_complete=false WHERE id=$1`, c.MessageID); err != nil {
			return 0, err
		}
	}
	if campaignEmail(c) && (uncertain || recorded != c.Attempt-1) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, errEmailAcceptanceUnknown
	}
	bulk := campaignEmail(c)
	for _, window := range []struct {
		duration       time.Duration
		total, reserve int
	}{{time.Minute, p.limits.PerMinute, p.limits.ReserveMinute}, {time.Hour, p.limits.PerHour, p.limits.ReserveHour}} {
		var total, bulkCount int
		var oldest, oldestBulk sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE bulk),min(started_at),min(started_at) FILTER(WHERE bulk) FROM notification_email_attempts WHERE outcome<>'deferred' AND started_at>$1`, now.Add(-window.duration)).Scan(&total, &bulkCount, &oldest, &oldestBulk)
		if err != nil {
			return 0, err
		}
		if total >= window.total && oldest.Valid && oldest.Time.Add(window.duration).After(next) {
			next = oldest.Time.Add(window.duration)
		}
		if bulk && bulkCount >= window.total-window.reserve && oldestBulk.Valid && oldestBulk.Time.Add(window.duration).After(next) {
			next = oldestBulk.Time.Add(window.duration)
		}
	}
	if cooldown.After(next) {
		next = cooldown
	}
	delay := next.Sub(now)
	outcome := "started"
	if delay > 0 {
		outcome = "deferred"
	} else {
		delay = 0
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider,finished_at) VALUES($1,$2,$3,$4::text,$5::timestamptz,$6,CASE WHEN $4::text='deferred' THEN $5::timestamptz ELSE NULL END) ON CONFLICT(delivery_id,attempt) DO UPDATE SET outcome=EXCLUDED.outcome,started_at=EXCLUDED.started_at,finished_at=EXCLUDED.finished_at WHERE notification_email_attempts.outcome='deferred'`, c.DeliveryID, c.Attempt, bulk, outcome, now, name)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("email attempt fence lost")
	}
	if delay == 0 {
		perMinute := p.limits.PerMinute
		if bulk {
			perMinute -= p.limits.ReserveMinute
		}
		if _, err = tx.ExecContext(ctx, `UPDATE notification_email_budget SET next_send_at=$1 WHERE id=true`, now.Add(time.Minute/time.Duration(perMinute))); err != nil {
			return 0, err
		}
	}
	return delay, tx.Commit()
}

func (p *emailPolicy) finish(ctx context.Context, c claim, receipt providers.ProviderReceipt, providerErr error) error {
	outcome, stage, enhanced := "accepted", "data", ""
	if receipt.Provider == "" {
		receipt.Provider = p.provider
		if receipt.Provider == "" {
			receipt.Provider = "smtp"
		}
	}
	code := 250
	if receipt.Provider == "ses" {
		code = 0
	}
	var pe *providers.ProviderError
	if providerErr != nil {
		outcome = "unknown"
		if errors.As(providerErr, &pe) {
			stage, code, enhanced = pe.Operation, pe.SMTPCode, pe.EnhancedCode
			if pe.Kind != providers.ErrorAcceptanceUnknown {
				outcome = "rejected"
			}
		}
	}
	tx, err := beginDeliveryTransaction(ctx, p.db, p.conn)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Use the same lock order as begin, including during a provider cooldown update.
	var strikes int
	if err = tx.QueryRowContext(ctx, `SELECT throttle_count FROM notification_email_budget WHERE id=true FOR UPDATE`).Scan(&strikes); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_email_attempts SET outcome=$3,stage=$4,smtp_code=$5,enhanced_code=$6,finished_at=clock_timestamp(),provider=$7,provider_message_id=$8,provider_accepted_at=$9 WHERE delivery_id=$1 AND attempt=$2 AND outcome='started'`, c.DeliveryID, c.Attempt, outcome, stage, code, enhanced, receipt.Provider, nullableReceiptID(receipt.ProviderMessageID), nullableReceiptTime(receipt.AcceptedAt))
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("email attempt fence lost")
	}
	if providerErr == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE notification_email_budget SET throttle_count=0 WHERE id=true`); err != nil {
			return err
		}
	}
	if pe != nil && pe.Kind == providers.ErrorRateLimited {
		delay := pe.RetryAfter
		if delay <= 0 {
			delay = time.Minute * time.Duration(1<<min(strikes, 6))
			delay = min(delay, time.Hour)
			delay += time.Duration(rand.Int64N(int64(delay/4) + 1))
		}
		if _, err = tx.ExecContext(ctx, `UPDATE notification_email_budget SET cooldown_until=GREATEST(cooldown_until,clock_timestamp()+($1::bigint*interval '1 microsecond')),throttle_count=LEAST(throttle_count+1,6) WHERE id=true`, delay.Microseconds()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *emailPolicy) acceptedReceipt(ctx context.Context, c claim) (providers.ProviderReceipt, error) {
	var receipt providers.ProviderReceipt
	err := p.statements().QueryRowContext(ctx, `SELECT provider,COALESCE(provider_message_id,'<'||delivery_id::text||'@notification.alive.org.tw>'),COALESCE(provider_accepted_at,finished_at) FROM notification_email_attempts WHERE delivery_id=$1 AND outcome='accepted' ORDER BY attempt DESC LIMIT 1`, c.DeliveryID).Scan(&receipt.Provider, &receipt.ProviderMessageID, &receipt.AcceptedAt)
	return receipt, err
}

func nullableReceiptID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
func nullableReceiptTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

func (p *emailPolicy) observe(ctx context.Context, c claim) error {
	_, err := p.statements().ExecContext(ctx, `INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,finished_at) VALUES($1,$2,$3,'deferred',clock_timestamp()) ON CONFLICT DO NOTHING`, c.DeliveryID, c.Attempt, campaignEmail(c))
	return err
}

// Recover recorded acceptance before expiry, consent or payload checks; these cannot undo a completed send.
func restoreAcceptedEmail(ctx context.Context, tx *database.SessionTransaction, deliveryID string) (bool, error) {
	var messageID string
	var acceptedAt time.Time
	var providerMessageID string
	err := tx.QueryRowContext(ctx, `SELECT d.message_id::text,COALESCE(a.provider_accepted_at,a.finished_at),COALESCE(a.provider_message_id,'<'||d.id::text||'@notification.alive.org.tw>') FROM notification_deliveries d JOIN notification_email_attempts a ON a.delivery_id=d.id WHERE d.id=$1 AND (d.status IN ('queued','sending') OR (d.status='dead_lettered' AND d.last_error_code='expired')) AND a.outcome='accepted' ORDER BY a.attempt DESC LIMIT 1 FOR UPDATE OF d`, deliveryID).Scan(&messageID, &acceptedAt, &providerMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='sent',sent_at=$2,provider_message_id=$3,last_error_code=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, deliveryID, acceptedAt, providerMessageID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_messages SET status='sent',terminal_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, messageID); err != nil {
		return false, err
	}
	return true, nil
}
