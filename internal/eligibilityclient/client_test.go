package eligibilityclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func TestPrivateEligibilityPreservesContractAndFailsClosed(t *testing.T) {
	token := strings.Repeat("a", 64)
	ref := contracts.EligibilityRef{CampaignID: "campaign", RecipientID: "recipient"}
	for _, tt := range []struct {
		name, body string
		code       int
		allow, err bool
	}{
		{"allow", `{"data":{"decision":"allow"}}`, 200, true, false},
		{"suppress", `{"data":{"decision":"suppress"}}`, 200, false, false},
		{"unauthorized", "", 403, false, true},
		{"unavailable", "", 503, false, true},
		{"invalid", `{"data":{"decision":"unknown"}}`, 200, false, true},
		{"conflicting error", `{"data":{"decision":"allow"},"error":{"code":"UNAVAILABLE"}}`, 200, false, true},
		{"trailing", `{"data":{"decision":"allow"}}{}`, 200, false, true},
		{"redirect", "", 307, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/unexpected" {
					t.Error("redirect was followed")
					return
				}
				var got contracts.EligibilityRef
				if r.Method != http.MethodPost || r.URL.Path != "/priv/notification-worker/campaign-deliveries/eligibility" ||
					r.Header.Get("X-HHC-Notification-Worker-Eligibility-Token") != token ||
					r.Header.Get("Dapr-Caller-App-Id") != "" || json.NewDecoder(r.Body).Decode(&got) != nil || got != ref {
					t.Error("private eligibility request violated the contract")
				}
				w.Header().Set("Location", "/unexpected")
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := New(token)
			if client.baseURL != "https://engagement-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io" {
				t.Fatal("unexpected private eligibility origin")
			}
			client.baseURL = server.URL
			client.http.Transport = server.Client().Transport
			allowed, err := client.Check(context.Background(), ref)
			if allowed != tt.allow || (err != nil) != tt.err {
				t.Fatalf("allowed = %t, error = %v", allowed, err)
			}
		})
	}
}
