package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
)

var errEmailAcceptanceUnknown = errors.New("email acceptance requires reconciliation")
var errEmailSuppressed = errors.New("email is technically suppressed")
var errEmailProviderMismatch = errors.New("email provider does not match pinned delivery")

var errEmailAlreadyAccepted = errors.New("email already accepted")

type EmailLimits struct {
	PerMinute, PerHour, ReserveMinute, ReserveHour int
	GraphDailyBudget, GraphDailyReserve            int
}

const (
	graphPerMinute      = 25
	graphReserveMinute  = 5
	graphSendInterval   = time.Minute / graphPerMinute
	graphPermitLifetime = time.Second
	graphFenceKey       = "notification-graph-mailbox"
)

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

func campaignEmail(c claim) bool {
	return templates.DeliveryClassFor(c.TemplateID, c.TemplateVersion) == templates.DeliveryClassBulk
}

func (p *emailPolicy) budgetTable() string {
	if p.provider == "graph" {
		return "notification_graph_email_budget"
	}
	return "notification_email_budget"
}

func (p *emailPolicy) lockGraphMailbox(ctx context.Context) (func(), error) {
	if p.conn == nil {
		return nil, errors.New("Graph mailbox fence requires an owned delivery session")
	}
	var locked bool
	if err := p.conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, graphFenceKey).Scan(&locked); err != nil {
		_ = p.conn.Raw(func(any) error { return driver.ErrBadConn })
		return nil, err
	}
	if !locked {
		return nil, nil
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if _, err := p.conn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, graphFenceKey); err != nil {
			_ = p.conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}, nil
}

// A late worker must release its reservation before any call reaches the provider.
func (p *emailPolicy) confirmGraphPermit(ctx context.Context, c claim) (bool, error) {
	var allowed bool
	err := p.statements().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM notification_email_attempts a
			JOIN notification_deliveries d ON d.id=a.delivery_id
			JOIN notification_messages m ON m.id=d.message_id
			WHERE d.id=$1 AND d.provider='graph' AND d.status='sending'
			  AND d.graph_mailbox_id=NULLIF($4,'')::uuid
			  AND d.attempt_count=$2 AND a.attempt=$2 AND a.outcome='started'
			  AND d.lease_expires_at IS NOT NULL
			  AND LEAST(a.started_at+($3::bigint*interval '1 microsecond'),d.lease_expires_at,m.expires_at)>clock_timestamp()
		)`, c.DeliveryID, c.Attempt, graphPermitLifetime.Microseconds(), c.GraphMailboxID).Scan(&allowed)
	if err != nil || allowed {
		return allowed, err
	}
	_, err = p.statements().ExecContext(ctx, `
		WITH deferred AS (
			UPDATE notification_email_attempts SET outcome='deferred',finished_at=clock_timestamp()
			WHERE delivery_id=$1 AND attempt=$2 AND outcome='started' RETURNING 1
		)
		UPDATE notification_graph_email_budget SET next_send_at=clock_timestamp()
		WHERE id=true AND EXISTS(SELECT 1 FROM deferred)`, c.DeliveryID, c.Attempt)
	return false, err
}

func (p *emailPolicy) begin(ctx context.Context, c claim) (time.Duration, error) {
	graph := p.provider == "graph"
	if graph {
		if p.limits.GraphDailyBudget <= 0 || p.limits.GraphDailyReserve < 0 || p.limits.GraphDailyReserve >= p.limits.GraphDailyBudget {
			return 0, errors.New("Graph daily budget must be positive and its reserve smaller than the budget")
		}
	} else if err := p.limits.Validate(); err != nil {
		return 0, err
	}
	tx, err := beginDeliveryTransaction(ctx, p.db, p.conn)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var now, next, cooldown time.Time
	var halted bool
	haltColumn := "false"
	if graph {
		haltColumn = "configuration_halted"
	}
	if err = tx.QueryRowContext(ctx, `SELECT next_send_at,cooldown_until,`+haltColumn+` FROM `+p.budgetTable()+` WHERE id=true FOR UPDATE`).Scan(&next, &cooldown, &halted); err != nil {
		return 0, err
	}
	if halted {
		return 5 * time.Minute, nil
	}
	// A clock evaluated before the lock wait can expire a reservation before admission.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
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
	bulk := campaignEmail(c)
	if bulk && (uncertain || recorded != c.Attempt-1) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, errEmailAcceptanceUnknown
	}
	windows := [2]struct {
		duration       time.Duration
		total, reserve int
	}{{time.Minute, p.limits.PerMinute, p.limits.ReserveMinute}, {time.Hour, p.limits.PerHour, p.limits.ReserveHour}}
	providerFilter := "provider<>'graph'"
	if graph {
		providerFilter = "provider='graph'"
		// Count a reservation through its latest permitted submission instant.
		windows[0].duration, windows[0].total, windows[0].reserve = time.Minute+graphPermitLifetime, graphPerMinute, graphReserveMinute
		windows[1].duration, windows[1].total, windows[1].reserve = 24*time.Hour+graphPermitLifetime, p.limits.GraphDailyBudget, p.limits.GraphDailyReserve
	}
	for _, window := range windows {
		var total, bulkCount int
		var oldest, oldestBulk sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE bulk),min(started_at),min(started_at) FILTER(WHERE bulk) FROM notification_email_attempts WHERE outcome<>'deferred' AND started_at>$1 AND `+providerFilter, now.Add(-window.duration)).Scan(&total, &bulkCount, &oldest, &oldestBulk)
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
	result, err := tx.ExecContext(ctx, `INSERT INTO notification_email_attempts(delivery_id,attempt,bulk,outcome,started_at,provider,finished_at) VALUES($1,$2,$3,$4::text,$5::timestamptz,$6,CASE WHEN $4::text='deferred' THEN $5::timestamptz ELSE NULL END) ON CONFLICT(delivery_id,attempt) DO UPDATE SET outcome=EXCLUDED.outcome,started_at=EXCLUDED.started_at,provider=EXCLUDED.provider,finished_at=EXCLUDED.finished_at WHERE notification_email_attempts.outcome='deferred'`, c.DeliveryID, c.Attempt, bulk, outcome, now, name)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("email attempt fence lost")
	}
	if delay == 0 {
		var interval time.Duration
		if graph {
			// Preserve the hold across connection loss or a crash with no durable finish.
			interval = graphPermitLifetime + sendTimeout + graphSendInterval
		} else {
			perMinute := p.limits.PerMinute
			if bulk {
				perMinute -= p.limits.ReserveMinute
			}
			interval = time.Minute / time.Duration(perMinute)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE `+p.budgetTable()+` SET next_send_at=$1 WHERE id=true`, now.Add(interval)); err != nil {
			return 0, err
		}
	}
	return delay, tx.Commit()
}

func (p *emailPolicy) finish(ctx context.Context, c claim, receipt providers.ProviderReceipt, providerErr error) error {
	outcome, stage, enhanced := "accepted", "data", ""
	family := p.provider
	if family == "" {
		family = "smtp"
	}
	if providerErr == nil && receipt.Provider != "" && receipt.Provider != family {
		return errors.New("accepted receipt does not match the pinned email provider")
	}
	code := 0
	if family == "smtp" {
		code = 250
	}
	requestID := receipt.ProviderRequestID
	var pe *providers.ProviderError
	if providerErr != nil {
		receipt = providers.ProviderReceipt{}
		requestID = ""
		outcome, code = "unknown", 0
		if errors.As(providerErr, &pe) {
			stage, code, enhanced = pe.Operation, pe.SMTPCode, pe.EnhancedCode
			requestID = pe.ProviderRequestID
			if pe.Kind != providers.ErrorAcceptanceUnknown {
				outcome = "rejected"
			}
		}
	}
	if !providerRequestIDPattern.MatchString(requestID) {
		requestID = ""
	}
	tx, err := beginDeliveryTransaction(ctx, p.db, p.conn)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Use the same lock order as begin, including during a provider cooldown update.
	var strikes int
	if err = tx.QueryRowContext(ctx, `SELECT throttle_count FROM `+p.budgetTable()+` WHERE id=true FOR UPDATE`).Scan(&strikes); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_email_attempts SET outcome=$3,stage=$4,smtp_code=$5,enhanced_code=$6,finished_at=clock_timestamp(),provider_message_id=$7,provider_accepted_at=$8,provider_request_id=$9 WHERE delivery_id=$1 AND attempt=$2 AND outcome='started' AND provider=$10`, c.DeliveryID, c.Attempt, outcome, stage, code, enhanced, nullableReceiptID(receipt.ProviderMessageID), nullableReceiptTime(receipt.AcceptedAt), nullableReceiptID(requestID), family)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("email attempt fence lost")
	}
	if providerErr == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE `+p.budgetTable()+` SET throttle_count=0 WHERE id=true`); err != nil {
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
		if _, err = tx.ExecContext(ctx, `UPDATE `+p.budgetTable()+` SET cooldown_until=GREATEST(cooldown_until,clock_timestamp()+($1::bigint*interval '1 microsecond')),throttle_count=LEAST(throttle_count+1,6) WHERE id=true`, delay.Microseconds()); err != nil {
			return err
		}
	}
	if p.provider == "graph" {
		halt := pe != nil && pe.HTTPStatus == http.StatusForbidden
		if _, err := tx.ExecContext(ctx, `UPDATE notification_graph_email_budget
			SET next_send_at=clock_timestamp()+($1::bigint*interval '1 microsecond'),
			    configuration_halted=configuration_halted OR $2
			WHERE id=true`, graphSendInterval.Microseconds(), halt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var providerRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (p *emailPolicy) acceptedReceipt(ctx context.Context, c claim) (providers.ProviderReceipt, error) {
	var receipt providers.ProviderReceipt
	err := p.statements().QueryRowContext(ctx, `SELECT provider,COALESCE(provider_message_id,CASE WHEN provider='smtp' THEN '<'||delivery_id::text||'@notification.alive.org.tw>' ELSE '' END),COALESCE(provider_request_id,''),COALESCE(provider_accepted_at,finished_at) FROM notification_email_attempts WHERE delivery_id=$1 AND outcome='accepted' ORDER BY attempt DESC LIMIT 1`, c.DeliveryID).Scan(&receipt.Provider, &receipt.ProviderMessageID, &receipt.ProviderRequestID, &receipt.AcceptedAt)
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
	err := tx.QueryRowContext(ctx, `SELECT d.message_id::text,COALESCE(a.provider_accepted_at,a.finished_at),COALESCE(a.provider_message_id,CASE WHEN a.provider='smtp' THEN '<'||d.id::text||'@notification.alive.org.tw>' ELSE '' END) FROM notification_deliveries d JOIN notification_email_attempts a ON a.delivery_id=d.id WHERE d.id=$1 AND (d.status IN ('queued','sending') OR (d.status='dead_lettered' AND d.last_error_code='expired')) AND a.outcome='accepted' ORDER BY a.attempt DESC LIMIT 1 FOR UPDATE OF d`, deliveryID).Scan(&messageID, &acceptedAt, &providerMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='sent',sent_at=$2,provider_message_id=NULLIF($3,''),last_error_code=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, deliveryID, acceptedAt, providerMessageID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_messages SET status='sent',terminal_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, messageID); err != nil {
		return false, err
	}
	return true, nil
}
