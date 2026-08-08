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
	"regexp"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	maxWebPushResponseBodyBytes = 1024
	maxWebPushProviderReasonLen = 256
)

var (
	webPushSensitiveDetail = regexp.MustCompile(`(?i)(["']?[A-Z0-9_. -]*(?:endpoint|token|p256dh|auth(?:orization)?|private(?:[_ -]?key)?)[A-Z0-9_. -]*["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,}\]]+)`)
	webPushEmailDetail     = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	webPushURLDetail       = regexp.MustCompile(`https?://[^\s"'<>]+`)
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
		Title         string `json:"title"`
		Body          string `json:"body"`
		ClickBehavior string `json:"clickBehavior"`
		ActionURL     string `json:"actionUrl,omitempty"`
	}{payload.Title, payload.Body, payload.ClickBehavior, payload.ActionURL})
	if err != nil {
		return ProviderReceipt{}, w.failed(ErrorPermanent, "encode", err)
	}
	response, err := w.send(ctx, message, &subscription, &webpush.Options{
		Subscriber: webPushSubscriber(w.config.Subject), VAPIDPublicKey: w.config.PublicKey,
		VAPIDPrivateKey: w.config.PrivateKey, TTL: 24 * 60 * 60,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ProviderReceipt{}, w.failed(ErrorTemporary, "send", ctx.Err())
		}
		return ProviderReceipt{}, w.failed(ErrorTemporary, "send", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		providerErr := classifyWebPushResponse(response, webPushProviderFamily(subscription.Endpoint))
		w.logFailure(providerErr)
		return ProviderReceipt{}, providerErr
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if w.config.Logger != nil {
		w.config.Logger.Print("event=notification_provider_success provider=webpush")
	}
	return ProviderReceipt{Provider: "webpush", AcceptedAt: time.Now().UTC()}, nil
}

func webPushSubscriber(subject string) string {
	return strings.TrimPrefix(subject, "mailto:")
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

func classifyWebPushResponse(response *http.Response, providerFamily string) *ProviderError {
	kind := ErrorPermanent
	switch {
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		kind = ErrorInvalidEndpoint
	case response.StatusCode == http.StatusTooManyRequests:
		kind = ErrorRateLimited
	case response.StatusCode >= 500:
		kind = ErrorTemporary
	}
	providerErr := &ProviderError{
		Kind:              kind,
		Operation:         "send",
		RetryAfter:        parseRetryAfter(response.Header.Get("Retry-After")),
		HTTPStatus:        response.StatusCode,
		ProviderRequestID: webPushRequestID(response.Header),
		ProviderFamily:    providerFamily,
		ProviderReason:    webPushProviderReason(response.Body),
	}
	providerErr.cause = fmt.Errorf(
		"web push provider returned HTTP %d family=%s request_id=%q reason=%q",
		providerErr.HTTPStatus,
		providerErr.ProviderFamily,
		providerErr.ProviderRequestID,
		providerErr.ProviderReason,
	)
	return providerErr
}

func webPushProviderFamily(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "unknown"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "web.push.apple.com" || strings.HasSuffix(host, ".push.apple.com"):
		return "apple"
	case host == "fcm.googleapis.com" || strings.HasSuffix(host, ".fcm.googleapis.com"):
		return "fcm"
	case host == "updates.push.services.mozilla.com" || strings.HasSuffix(host, ".push.services.mozilla.com"):
		return "mozilla"
	default:
		return "unknown"
	}
}

func webPushRequestID(header http.Header) string {
	for _, name := range []string{"Apns-Id", "X-Request-Id", "Request-Id", "X-Amzn-Requestid", "X-Amz-Request-Id", "X-Goog-Request-Id"} {
		if value := sanitizeWebPushDetail(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func webPushProviderReason(body io.Reader) string {
	if body == nil {
		return ""
	}
	contents, err := io.ReadAll(io.LimitReader(body, maxWebPushResponseBodyBytes))
	if err != nil {
		return ""
	}
	return sanitizeWebPushDetail(string(contents))
}

func sanitizeWebPushDetail(detail string) string {
	detail = strings.ToValidUTF8(detail, "")
	detail = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, detail)
	detail = webPushSensitiveDetail.ReplaceAllString(detail, "$1[redacted]")
	detail = webPushEmailDetail.ReplaceAllString(detail, "[redacted-email]")
	detail = webPushURLDetail.ReplaceAllString(detail, "[redacted-url]")
	detail = strings.Join(strings.Fields(detail), " ")
	characters := []rune(detail)
	if len(characters) > maxWebPushProviderReasonLen {
		return string(characters[:maxWebPushProviderReasonLen])
	}
	return detail
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
	providerErr := &ProviderError{Kind: kind, Operation: operation, cause: cause}
	w.logFailure(providerErr)
	return providerErr
}

func (w *WebPush) logFailure(providerErr *ProviderError) {
	if w.config.Logger != nil {
		w.config.Logger.Printf(
			"event=notification_provider_failure provider=webpush kind=%s operation=%s http_status=%d provider_family=%s provider_request_id=%q provider_reason=%q",
			providerErr.Kind,
			providerErr.Operation,
			providerErr.HTTPStatus,
			providerErr.ProviderFamily,
			providerErr.ProviderRequestID,
			providerErr.ProviderReason,
		)
	}
}
