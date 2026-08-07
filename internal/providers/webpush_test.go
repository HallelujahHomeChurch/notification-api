package providers

import (
	"context"
	"errors"
	"io"
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
			err := classifyWebPushResponse(&http.Response{StatusCode: test.status, Header: header})
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != test.kind ||
				providerErr.RetryAfter != test.retry || providerErr.Retryable() != test.retryable {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}
