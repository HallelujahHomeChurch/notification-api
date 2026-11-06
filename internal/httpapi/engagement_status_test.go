package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/service"
)

func TestEngagementStatusListener(t *testing.T) {
	const id = "6d387ca2-dfa0-4713-8fa5-490c1c9f8304"
	const path = "/priv/notifications/" + id
	const tokenHeader = "X-HHC-Notification-Engagement-Token"
	token := strings.Repeat("a", 64)
	svc := &fakeService{get: func(_ context.Context, caller, messageID string) (service.Result, error) {
		if caller != "engagement-api" || messageID != id {
			t.Fatalf("unexpected status lookup: %q %q", caller, messageID)
		}
		return service.Result{MessageID: id, Status: "failed", FailureCode: "example", SafeToRetry: true}, nil
	}}
	direct := NewPrivateHandler(svc, nil, PrivateCredentials{EngagementStatus: token})
	dapr := New(svc, nil, []string{"engagement-api"}, false)
	for _, tt := range []struct {
		name, method, path string
		tokens             []string
		injected           string
		want               int
	}{
		{"valid", "GET", path, []string{token}, "", 200},
		{"missing credential", "GET", path, nil, "", 403},
		{"empty credential", "GET", path, []string{""}, "", 403},
		{"wrong credential", "GET", path, []string{strings.Repeat("b", 64)}, "", 403},
		{"duplicate credential", "GET", path, []string{token, token}, "", 403},
		{"forged Dapr caller", "GET", path, []string{token}, "Dapr-Caller-App-Id", 403},
		{"empty caller header", "GET", path, []string{token}, "empty", 403},
		{"forged dev caller", "GET", path, []string{token}, "X-HHC-Caller-App-Id", 403},
		{"forged internal caller", "GET", path, []string{token}, "X-Internal-Caller-App-Id", 403},
		{"Dapr API token", "GET", path, []string{token}, "Dapr-Api-Token", 403},
		{"HEAD", "HEAD", path, []string{token}, "", 405},
		{"POST", "POST", path, []string{token}, "", 405},
		{"enqueue", "POST", "/priv/notifications/send", []string{token}, "", 404},
		{"cleanup", "POST", "/priv/account-cleanup", []string{token}, "", 404},
		{"invalid ID", "GET", "/priv/notifications/invalid", []string{token}, "", 404},
		{"encoded path", "GET", "/priv/notifications/%36" + id[1:], []string{token}, "", 404},
		{"health", "GET", "/health", []string{token}, "", 404},
		{"trailing slash", "GET", path + "/", []string{token}, "", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			for _, token := range tt.tokens {
				r.Header.Add(tokenHeader, token)
			}
			if tt.injected == "empty" {
				r.Header.Set("Dapr-Caller-App-Id", "")
			} else if tt.injected != "" {
				r.Header.Set(tt.injected, "engagement-api")
			}
			before := svc.getCalls
			got := serve(direct, r)
			if got.Code != tt.want || (svc.getCalls > before) != (tt.want == 200) || svc.sendCalls != 0 {
				t.Fatalf("unexpected result: status=%d lookups=%d sends=%d", got.Code, svc.getCalls-before, svc.sendCalls)
			}
			if tt.want == 200 {
				daprRequest := httptest.NewRequest("GET", "/priv/notifications/"+id, nil)
				daprRequest.Header.Set("Dapr-Caller-App-Id", "engagement-api")
				daprResponse := serve(dapr, daprRequest)
				var directData, daprData responseEnvelope
				if err := json.Unmarshal(got.Body.Bytes(), &directData); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(daprResponse.Body.Bytes(), &daprData); err != nil {
					t.Fatal(err)
				}
				if daprResponse.Code != 200 || !reflect.DeepEqual(directData.Data, daprData.Data) {
					t.Fatalf("direct data = %v, Dapr data = %v", directData.Data, daprData.Data)
				}
			}
		})
	}
	request := httptest.NewRequest("GET", path, nil)
	request.Header.Set(tokenHeader, token)
	if got := serve(dapr, request); got.Code != 401 {
		t.Fatalf("direct token on Dapr listener: %d", got.Code)
	}
}
