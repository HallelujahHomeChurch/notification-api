package nativepush

import (
	"context"
	"database/sql"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"log/slog"
	"strings"
	"time"
)

// WebProvider reuses Operations eligibility and the existing Web Push transport.
type WebProvider struct {
	Operations *Provider
	Web        providers.Provider
}

func (p *WebProvider) Send(ctx context.Context, v providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	allowed, err := p.Operations.Eligibility(ctx, v)
	if err != nil {
		return providers.ProviderReceipt{}, err
	}
	v.TTL = allowed.TTL
	v.Title = "服事通知"
	v.Body = "你的服事有新的消息，請開啟服事表查看。"
	v.ClickBehavior = "url"
	v.ActionURL = "https://account.alive.org.tw/service/assignments/" + allowed.AssignmentID
	return p.Web.Send(ctx, v)
}

// The validated idempotency key retains callback identity after sensitive payload purging.
// A failed callback never retries provider delivery.
func (p *WebProvider) PollResults(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT d.id::text,m.idempotency_key,d.status,COALESCE(d.last_error_code,'') FROM notification_deliveries d JOIN notification_messages m ON m.id=d.message_id WHERE m.template_id='operations.web-push' AND d.status IN ('sent','failed','dead_lettered','suppressed') AND COALESCE(d.last_error_code,'') NOT LIKE 'service_reported_%' ORDER BY d.updated_at LIMIT 100`)
	if err != nil {
		return err
	}
	type outcome struct {
		id, idempotencyKey, status, code string
	}
	list := []outcome{}
	for rows.Next() {
		var v outcome
		if err = rows.Scan(&v.id, &v.idempotencyKey, &v.status, &v.code); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		result := "provider_failed"
		if v.status == "sent" {
			result = "provider_received"
		} else if v.code == "invalid_endpoint" {
			result = "invalid_endpoint"
		}
		if err = p.Operations.callback(ctx, strings.TrimPrefix(v.idempotencyKey, "service:"), result); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `UPDATE notification_deliveries SET last_error_code=$2 WHERE id=$1 AND status=$3 AND COALESCE(last_error_code,'')=$4`, v.id, "service_reported_"+result, v.status, v.code); err != nil {
			return err
		}
	}
	return nil
}
func (p *WebProvider) RunResults(ctx context.Context, db *sql.DB) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := p.PollResults(ctx, db); err != nil {
				slog.Warn("service web push results unavailable")
			}
		}
	}
}
