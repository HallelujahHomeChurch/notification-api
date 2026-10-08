package nativepush

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"net/http"
	"net/http/httptest"
	"testing"
)

type webProviderStub struct {
	calls   int
	payload providers.DeliveryPayload
}

func (p *webProviderStub) Send(_ context.Context, v providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	p.calls++
	p.payload = v
	return providers.ProviderReceipt{Provider: "webpush"}, nil
}
func TestServiceWebPushRechecksEligibilityAndBoundsTTL(t *testing.T) {
	allow := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/priv/service-push/eligibility" {
			t.Fatal("unexpected provider-time callback")
		}
		json.NewEncoder(w).Encode(map[string]any{"allow": allow, "ttl": 123, "assignmentId": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	web := &webProviderStub{}
	p := &WebProvider{Operations: New(server.URL, ""), Web: web}
	v := providers.DeliveryPayload{Recipient: "opaque subscription", MessageID: "delivery", ActionURL: "11111111-1111-4111-8111-111111111111"}
	_, err := p.Send(context.Background(), v)
	var pe *providers.ProviderError
	if !errors.As(err, &pe) || pe.Kind != providers.ErrorSuppressed || web.calls != 0 {
		t.Fatalf("sent stale service: %v", err)
	}
	allow = true
	_, err = p.Send(context.Background(), v)
	if err != nil || web.calls != 1 || web.payload.TTL != 123 || web.payload.ActionURL != "https://account.alive.org.tw/service/assignments/"+v.ActionURL || web.payload.Title != "服事通知" {
		t.Fatalf("wrong web payload: %+v %v", web.payload, err)
	}
}
