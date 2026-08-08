package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	testVAPIDPrivateKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE"
	testVAPIDPublicKey  = "BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"
)

func TestValidateWebPushConfigRequiresMatchingVAPIDKeys(t *testing.T) {
	valid := WebPushConfig{
		PublicKey: testVAPIDPublicKey, PrivateKey: testVAPIDPrivateKey,
		Subject: "mailto:support@alive.org.tw",
	}
	if err := ValidateWebPushConfig(valid); err != nil {
		t.Fatalf("ValidateWebPushConfig(valid) error = %v", err)
	}
	valid.Subject = "https://www.alive.org.tw"
	if err := ValidateWebPushConfig(valid); err != nil {
		t.Fatalf("ValidateWebPushConfig(HTTPS subject) error = %v", err)
	}
	valid.Subject = "mailto:support@alive.org.tw"
	valid.PrivateKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAI"
	if err := ValidateWebPushConfig(valid); err == nil {
		t.Fatal("ValidateWebPushConfig() accepted mismatched key pair")
	}
}

func TestWebPushSendsExpectedPayload(t *testing.T) {
	provider := NewWebPush(WebPushConfig{
		PublicKey: testVAPIDPublicKey, PrivateKey: testVAPIDPrivateKey, Subject: "mailto:support@alive.org.tw",
	})
	provider.send = func(_ context.Context, message []byte, subscription *webpush.Subscription, options *webpush.Options) (*http.Response, error) {
		if subscription.Endpoint != "https://push.example.test/subscription" ||
			!strings.Contains(string(message), `"title":"八月消息"`) ||
			!strings.Contains(string(message), `"actionUrl":"https://www.alive.org.tw/zh-Hant/news"`) ||
			options.Subscriber != "support@alive.org.tw" ||
			options.VAPIDPublicKey != testVAPIDPublicKey || options.VAPIDPrivateKey != testVAPIDPrivateKey {
			t.Fatalf("subscription=%#v payload=%s options=%#v", subscription, message, options)
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	receipt, err := provider.Send(context.Background(), DeliveryPayload{
		Recipient: `{"endpoint":"https://push.example.test/subscription","keys":{"p256dh":"BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU","auth":"AAAAAAAAAAAAAAAAAAAAAA"}}`,
		Title:     "八月消息", Body: "教會近況", ActionURL: "https://www.alive.org.tw/zh-Hant/news",
	})
	if err != nil || receipt.Provider != "webpush" || receipt.AcceptedAt.IsZero() {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
}

func TestWebPushSubscriberPreservesHTTPSSubject(t *testing.T) {
	const subject = "https://www.alive.org.tw"
	if got := webPushSubscriber(subject); got != subject {
		t.Fatalf("webPushSubscriber() = %q, want %q", got, subject)
	}
}

func TestWebPushFailurePreservesSafeProviderDetails(t *testing.T) {
	var logs bytes.Buffer
	provider := NewWebPush(WebPushConfig{
		PublicKey: testVAPIDPublicKey, PrivateKey: testVAPIDPrivateKey,
		Subject: "mailto:support@alive.org.tw", Logger: log.New(&logs, "", 0),
	})
	provider.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Apns-Id": []string{"apple-request-1"}},
			Body: io.NopCloser(strings.NewReader(
				`{"reason":"BadJwtToken","endpoint":"https://secret.example.test/subscription","token":"secret-token","deviceToken":"device-token-secret","p256dh":"p256dh-secret","auth":"auth-secret","private_key":"private-key-secret","email":"support@alive.org.tw","authorization":"Bearer private-token"}`,
			)),
		}, nil
	}

	_, err := provider.Send(context.Background(), DeliveryPayload{
		Recipient: `{"endpoint":"https://web.push.apple.com/subscription","keys":{"p256dh":"BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU","auth":"AAAAAAAAAAAAAAAAAAAAAA"}}`,
	})
	if err == nil {
		t.Fatal("Send() error = nil")
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("Send() error = %T, want *ProviderError", err)
	}
	if providerErr.HTTPStatus != http.StatusBadRequest ||
		providerErr.ProviderFamily != "apple" ||
		providerErr.ProviderRequestID != "apple-request-1" ||
		!strings.Contains(providerErr.ProviderReason, "BadJwtToken") {
		t.Fatalf("Send() provider error = %#v", providerErr)
	}
	for _, secret := range []string{
		"secret.example.test", "secret-token", "device-token-secret", "p256dh-secret", "auth-secret", "private-key-secret", "support@alive.org.tw", "private-token",
	} {
		if strings.Contains(providerErr.ProviderReason, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("provider details leaked %q: error=%#v logs=%q", secret, providerErr, logs.String())
		}
	}
	for _, detail := range []string{
		"http_status=400", "provider_family=apple", `provider_request_id="apple-request-1"`, "BadJwtToken",
	} {
		if !strings.Contains(logs.String(), detail) {
			t.Fatalf("provider details missing %q: logs=%q", detail, logs.String())
		}
	}
}

func TestWebPushProviderReasonIsBounded(t *testing.T) {
	reason := webPushProviderReason(strings.NewReader(strings.Repeat("x", 512)))
	if len([]rune(reason)) > 256 {
		t.Fatalf("provider reason length = %d, want at most 256", len([]rune(reason)))
	}
}

func TestWebPushClassifiesResponses(t *testing.T) {
	for _, test := range []struct {
		status    int
		kind      ErrorKind
		retry     time.Duration
		retryable bool
	}{
		{http.StatusNotFound, ErrorInvalidEndpoint, 0, false},
		{http.StatusGone, ErrorInvalidEndpoint, 0, false},
		{http.StatusTooManyRequests, ErrorRateLimited, 90 * time.Second, true},
		{http.StatusInternalServerError, ErrorTemporary, 0, true},
		{http.StatusBadRequest, ErrorPermanent, 0, false},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			header := http.Header{}
			if test.status == http.StatusTooManyRequests {
				header.Set("Retry-After", "90")
			}
			err := classifyWebPushResponse(&http.Response{StatusCode: test.status, Header: header}, "unknown")
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != test.kind ||
				providerErr.RetryAfter != test.retry || providerErr.Retryable() != test.retryable {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}
