// Package nativepush connects the existing durable notification worker to Expo.
// Pushes contain only an opaque resource reference and generic copy.
package nativepush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

type Provider struct {
	HTTP                                *http.Client
	ExpoURL, OperationsURL, AccessToken string
}

func New(operationsURL, token string) *Provider {
	return &Provider{HTTP: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, ExpoURL: "https://exp.host/--/api/v2/push", OperationsURL: operationsURL, AccessToken: token}
}
func (p *Provider) post(ctx context.Context, url string, in, out any, expo bool) error {
	b, e := json.Marshal(in)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if expo && p.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.AccessToken)
	}
	res, e := p.HTTP.Do(req)
	if e != nil {
		return &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "native_push"}
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		kind := providers.ErrorPermanent
		if res.StatusCode >= 500 || res.StatusCode == 429 {
			kind = providers.ErrorTemporary
		}
		return &providers.ProviderError{Kind: kind, Operation: "native_push", HTTPStatus: res.StatusCode}
	}
	if json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(out) != nil {
		return &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "native_push_response"}
	}
	return nil
}

type ticket struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

func (p *Provider) callback(ctx context.Context, id, result string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	if e := p.post(ctx, p.OperationsURL+"/priv/service-push/result", map[string]string{"deliveryId": id, "result": result}, &out, false); e != nil {
		return e
	}
	if !out.OK {
		return &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "native_callback"}
	}
	return nil
}
func (p *Provider) Send(ctx context.Context, v providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	var allowed struct {
		AssignmentID string `json:"assignmentId"`
		Allow        bool   `json:"allow"`
		TTL          int    `json:"ttl"`
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v.Recipient)))
	if e := p.post(ctx, p.OperationsURL+"/priv/service-push/eligibility", map[string]string{"deliveryId": v.MessageID, "tokenHash": hash}, &allowed, false); e != nil {
		return providers.ProviderReceipt{}, e
	}
	if !allowed.Allow || allowed.TTL <= 0 || allowed.AssignmentID != v.ActionURL {
		return providers.ProviderReceipt{}, &providers.ProviderError{Kind: providers.ErrorSuppressed, Operation: "native_eligibility"}
	}
	var response struct {
		Data ticket `json:"data"`
	}
	body := map[string]any{"to": v.Recipient, "title": "服事通知", "body": "你的服事有新的消息，請開啟 App 查看。", "sound": "default", "ttl": allowed.TTL, "data": map[string]string{"type": "service", "assignmentId": v.ActionURL}}
	if e := p.post(ctx, p.ExpoURL+"/send", body, &response, true); e != nil {
		return providers.ProviderReceipt{}, e
	}
	if response.Data.Status != "ok" || response.Data.ID == "" {
		kind := providers.ErrorPermanent
		if response.Data.Details.Error == "DeviceNotRegistered" {
			if e := p.callback(ctx, v.MessageID, "invalid_endpoint"); e != nil {
				return providers.ProviderReceipt{}, e
			}
			kind = providers.ErrorInvalidEndpoint
		}
		if response.Data.Details.Error == "MessageRateExceeded" {
			kind = providers.ErrorRateLimited
		}
		return providers.ProviderReceipt{}, &providers.ProviderError{Kind: kind, Operation: "native_ticket"}
	}
	return providers.ProviderReceipt{Provider: "expo", ProviderMessageID: response.Data.ID, AcceptedAt: time.Now().UTC()}, nil
}

// PollReceipts distinguishes Expo acceptance from APNs/FCM receipt. The existing
// delivery row is the durable receipt queue; callbacks are idempotent.
func (p *Provider) PollReceipts(ctx context.Context, db *sql.DB, keys map[string][]byte) error {
	rows, e := db.QueryContext(ctx, `SELECT d.id::text,d.provider_message_id,m.id::text,m.encryption_key_id,m.payload_ciphertext,d.sent_at FROM notification_deliveries d JOIN notification_messages m ON m.id=d.message_id WHERE d.provider='expo' AND d.status='sent' AND d.last_error_code IS NULL AND d.sent_at<now()-interval '15 minutes' AND m.payload_purged_at IS NULL ORDER BY d.sent_at LIMIT 100`)
	if e != nil {
		return e
	}
	type pending struct {
		id, ticket, message, key string
		payload                  []byte
		sent                     time.Time
	}
	list := []pending{}
	for rows.Next() {
		var v pending
		if e = rows.Scan(&v.id, &v.ticket, &v.message, &v.key, &v.payload, &v.sent); e != nil {
			rows.Close()
			return e
		}
		list = append(list, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range list {
		raw, e := notificationcrypto.DecryptWithKeyID(keys, v.key, []byte(v.message+":payload"), v.payload)
		if e != nil {
			return e
		}
		var envelope struct {
			Fields map[string]string `json:"fields"`
		}
		if e = json.Unmarshal(raw, &envelope); e != nil {
			return e
		}
		var response struct {
			Data map[string]ticket `json:"data"`
		}
		if e = p.post(ctx, p.ExpoURL+"/getReceipts", map[string]any{"ids": []string{v.ticket}}, &response, true); e != nil {
			return e
		}
		receipt, ok := response.Data[v.ticket]
		if !ok && time.Since(v.sent) < 23*time.Hour {
			continue
		}
		result := "receipt_unknown"
		if ok && receipt.Status == "ok" {
			result = "provider_received"
		} else if receipt.Details.Error == "DeviceNotRegistered" {
			result = "invalid_endpoint"
		} else if ok {
			result = "provider_failed"
		}
		if e = p.callback(ctx, envelope.Fields["deliveryId"], result); e != nil {
			return e
		}
		if _, e = db.ExecContext(ctx, `UPDATE notification_deliveries SET last_error_code=$2 WHERE id=$1 AND last_error_code IS NULL`, v.id, "native_"+result); e != nil {
			return e
		}
	}
	return nil
}
func (p *Provider) RunReceipts(ctx context.Context, db *sql.DB, keys map[string][]byte) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if e := p.PollReceipts(ctx, db, keys); e != nil {
				slog.Warn("native push receipts unavailable")
			}
		}
	}
}
