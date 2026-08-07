package providers

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type WebPushConfig struct {
	PublicKey  string
	PrivateKey string
	Subject    string
	Logger     *log.Logger
}

type WebPush struct {
	config WebPushConfig
	send   func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error)
}

func NewWebPush(config WebPushConfig) *WebPush {
	return &WebPush{config: config, send: webpush.SendNotificationWithContext}
}

func ValidateWebPushConfig(config WebPushConfig) error {
	if strings.TrimSpace(config.PublicKey) == "" || strings.TrimSpace(config.PrivateKey) == "" {
		return errors.New("VAPID public and private keys are required")
	}
	publicKey, publicErr := base64.RawURLEncoding.DecodeString(config.PublicKey)
	privateKey, privateErr := base64.RawURLEncoding.DecodeString(config.PrivateKey)
	privateScalar := new(big.Int).SetBytes(privateKey)
	if publicErr != nil || privateErr != nil || len(privateKey) != 32 ||
		privateScalar.Sign() <= 0 || privateScalar.Cmp(elliptic.P256().Params().N) >= 0 {
		return errors.New("VAPID key pair is invalid")
	}
	x, y := elliptic.P256().ScalarBaseMult(privateKey)
	if !bytes.Equal(publicKey, elliptic.Marshal(elliptic.P256(), x, y)) {
		return errors.New("VAPID public and private keys do not match")
	}
	subject, err := url.Parse(config.Subject)
	if err != nil || subject.Scheme != "mailto" && subject.Scheme != "https" ||
		subject.Scheme == "mailto" && subject.Opaque == "" || subject.Scheme == "https" && subject.Host == "" {
		return errors.New("VAPID subject must be a mailto or HTTPS URL")
	}
	return nil
}

func (w *WebPush) Send(ctx context.Context, payload DeliveryPayload) (ProviderReceipt, error) {
	var subscription webpush.Subscription
	if err := json.Unmarshal([]byte(payload.Recipient), &subscription); err != nil || !validSubscription(subscription) {
		return ProviderReceipt{}, w.failed(ErrorInvalidEndpoint, "validate", errors.New("invalid web push subscription"))
	}
	message, err := json.Marshal(struct {
		Title     string `json:"title"`
		Body      string `json:"body"`
		ActionURL string `json:"actionUrl"`
	}{payload.Title, payload.Body, payload.ActionURL})
	if err != nil {
		return ProviderReceipt{}, w.failed(ErrorPermanent, "encode", err)
	}
	response, err := w.send(ctx, message, &subscription, &webpush.Options{
		Subscriber: w.config.Subject, VAPIDPublicKey: w.config.PublicKey,
		VAPIDPrivateKey: w.config.PrivateKey, TTL: 24 * 60 * 60,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ProviderReceipt{}, w.failed(ErrorTemporary, "send", ctx.Err())
		}
		return ProviderReceipt{}, w.failed(ErrorTemporary, "send", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ProviderReceipt{}, classifyWebPushResponse(response)
	}
	if w.config.Logger != nil {
		w.config.Logger.Print("event=notification_provider_success provider=webpush")
	}
	return ProviderReceipt{Provider: "webpush", AcceptedAt: time.Now().UTC()}, nil
}

func validSubscription(subscription webpush.Subscription) bool {
	endpoint, err := url.Parse(subscription.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return false
	}
	p256dh, err := base64.RawURLEncoding.DecodeString(subscription.Keys.P256dh)
	if err != nil || len(p256dh) != 65 {
		return false
	}
	if x, _ := elliptic.Unmarshal(elliptic.P256(), p256dh); x == nil {
		return false
	}
	auth, err := base64.RawURLEncoding.DecodeString(subscription.Keys.Auth)
	return err == nil && len(auth) == 16
}

func classifyWebPushResponse(response *http.Response) error {
	kind := ErrorPermanent
	switch {
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		kind = ErrorInvalidEndpoint
	case response.StatusCode == http.StatusTooManyRequests:
		kind = ErrorRateLimited
	case response.StatusCode >= 500:
		kind = ErrorTemporary
	}
	return &ProviderError{
		Kind: kind, Operation: "send", RetryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
		cause: fmt.Errorf("web push service returned HTTP %d", response.StatusCode),
	}
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if delay := time.Until(deadline); delay > 0 {
			return delay
		}
	}
	return 0
}

func (w *WebPush) failed(kind ErrorKind, operation string, cause error) error {
	if w.config.Logger != nil {
		w.config.Logger.Printf("event=notification_provider_failure provider=webpush kind=%s operation=%s", kind, operation)
	}
	return &ProviderError{Kind: kind, Operation: operation, cause: cause}
}
