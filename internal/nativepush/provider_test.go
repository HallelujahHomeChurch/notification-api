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

func TestNativePushChecksEligibilityAndUsesOpaquePayload(t *testing.T) {
	allow := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/priv/service-push/eligibility":
			json.NewEncoder(w).Encode(map[string]any{"allow": allow, "ttl": 240, "assignmentId": "assignment"})
		case "/send":
			calls++
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid body")
			}
			if body["title"] != "服事通知" || body["ttl"] != float64(240) {
				t.Errorf("unexpected payload %v", body)
			}
			data := body["data"].(map[string]any)
			if len(data) != 2 || data["assignmentId"] != "assignment" {
				t.Error(data)
			}
			w.Write([]byte(`{"data":{"status":"ok","id":"receipt-id"}}`))
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	p := New(server.URL, "")
	p.ExpoURL = server.URL
	v := providers.DeliveryPayload{Recipient: "ExpoPushToken[example-token]", MessageID: "job", ActionURL: "assignment"}
	_, e := p.Send(context.Background(), v)
	var pe *providers.ProviderError
	if !errors.As(e, &pe) || pe.Kind != providers.ErrorSuppressed || calls != 0 {
		t.Fatalf("%v %d", e, calls)
	}
	allow = true
	receipt, e := p.Send(context.Background(), v)
	if e != nil || receipt.ProviderMessageID != "receipt-id" || calls != 1 {
		t.Fatalf("%+v %v", receipt, e)
	}
}
func TestNativeInvalidTokenRevokesBeforeTerminalFailure(t *testing.T) {
	revoked := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/priv/service-push/eligibility":
			w.Write([]byte(`{"allow":true,"ttl":240,"assignmentId":"assignment"}`))
		case "/send":
			w.Write([]byte(`{"data":{"status":"error","details":{"error":"DeviceNotRegistered"}}}`))
		case "/priv/service-push/result":
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			revoked = v["deliveryId"] == "job" && v["result"] == "invalid_endpoint"
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer s.Close()
	p := New(s.URL, "")
	p.ExpoURL = s.URL
	_, e := p.Send(t.Context(), providers.DeliveryPayload{MessageID: "job", ActionURL: "assignment"})
	var pe *providers.ProviderError
	if !revoked || !errors.As(e, &pe) || pe.Kind != providers.ErrorInvalidEndpoint {
		t.Fatalf("revoked=%v err=%v", revoked, e)
	}
}
