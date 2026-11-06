package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/accountcleanup"
	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
)

type privateCleanupFunc func(context.Context, accountcleanup.AccountCleanupRequest) (accountcleanup.AccountCleanupResult, error)

func (f privateCleanupFunc) CleanupAccount(ctx context.Context, request accountcleanup.AccountCleanupRequest) (accountcleanup.AccountCleanupResult, error) {
	return f(ctx, request)
}

func TestPrivateWritesScopeAndDaprParity(t *testing.T) {
	const send = "/priv/notifications/send"
	const cleanup = "/priv/account-cleanup"
	const status = "/priv/notifications/6d387ca2-dfa0-4713-8fa5-490c1c9f8304"
	const account = "X-HHC-Notification-Account-Token"
	const engagementSend = "X-HHC-Notification-Engagement-Send-Token"
	const engagementStatus = "X-HHC-Notification-Engagement-Token"
	const cleanupBody = `{"userId":"6d387ca2-dfa0-4713-8fa5-490c1c9f8304","canonicalEmail":"member@example.test","idempotencyKey":"delete-1"}`
	creds := PrivateCredentials{Account: strings.Repeat("a", 64), EngagementSend: strings.Repeat("b", 64), EngagementStatus: strings.Repeat("c", 64)}
	auth := func(header, token string) http.Header { return http.Header{header: []string{token}} }

	for _, tt := range []struct {
		name, method, path, caller string
		headers                    http.Header
		status                     int
	}{
		{"Account enqueue", "POST", send, "account-api", auth(account, creds.Account), 202},
		{"Engagement enqueue", "POST", send, "engagement-api", auth(engagementSend, creds.EngagementSend), 202},
		{"Account cleanup", "POST", cleanup, "account-api", auth(account, creds.Account), 200},
		{"Engagement cannot cleanup", "POST", cleanup, "", auth(engagementSend, creds.EngagementSend), 403},
		{"status cannot enqueue", "POST", send, "", auth(engagementStatus, creds.EngagementStatus), 403},
		{"send cannot read status", "GET", status, "", auth(engagementSend, creds.EngagementSend), 403},
		{"Account cannot read status", "GET", status, "", auth(account, creds.Account), 403},
		{"missing credential", "POST", send, "", nil, 403},
		{"wrong credential", "POST", send, "", auth(engagementSend, creds.Account), 403},
		{"duplicate credential", "POST", send, "", http.Header{account: {creds.Account, creds.Account}}, 403},
		{"two private scopes", "POST", send, "", http.Header{account: {creds.Account}, engagementSend: {creds.EngagementSend}}, 403},
		{"empty additional scope", "POST", send, "", http.Header{account: {creds.Account}, engagementStatus: {""}}, 403},
		{"forged Dapr identity", "POST", send, "", http.Header{account: {creds.Account}, "Dapr-Caller-App-Id": {"account-api"}}, 403},
		{"forged internal identity", "POST", cleanup, "", http.Header{account: {creds.Account}, "X-Internal-Caller-App-Id": {"account-api"}}, 403},
		{"wrong method", "GET", send, "", auth(account, creds.Account), 405},
		{"encoded enqueue path", "POST", "/priv/notifications/%73end", "", auth(account, creds.Account), 404},
		{"cleanup slash", "POST", cleanup + "/", "", auth(account, creds.Account), 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{send: func(_ context.Context, caller, key string, request contracts.SendRequest) (service.Result, error) {
				if caller != tt.caller || key != "operation-1" || request.TemplateID == "" {
					t.Fatal("enqueue identity, idempotency key or contract changed")
				}
				return service.Result{MessageID: "message-1", Status: "queued", TemplateVersion: 1, Replayed: true}, nil
			}}
			cleanupCalls := 0
			cleaner := privateCleanupFunc(func(_ context.Context, request accountcleanup.AccountCleanupRequest) (accountcleanup.AccountCleanupResult, error) {
				cleanupCalls++
				if request.IdempotencyKey != "delete-1" || request.Email != "member@example.test" {
					t.Fatal("cleanup request contract changed")
				}
				return accountcleanup.AccountCleanupResult{Owner: "notification-api", Status: "pending", RecordCount: 2, RemainingCount: 1}, nil
			})
			request := func() *http.Request {
				body := validBody()
				if tt.path == cleanup {
					body = cleanupBody
				}
				r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(body))
				r.Header.Set("X-HHC-Request-ID", "312bd5f0516840428cf5622a8ececc2d")
				r.Header.Set("Idempotency-Key", "operation-1")
				return r
			}
			r := request()
			for name, values := range tt.headers {
				for _, value := range values {
					r.Header.Add(name, value)
				}
			}
			got := serve(NewPrivateHandler(svc, cleaner, creds), r)
			if got.Code != tt.status {
				t.Fatalf("status = %d, want %d", got.Code, tt.status)
			}
			if tt.caller == "" {
				if svc.sendCalls != 0 || svc.getCalls != 0 || cleanupCalls != 0 {
					t.Fatal("unauthorized request reached a business handler")
				}
				return
			}
			if svc.sendCalls+cleanupCalls != 1 {
				t.Fatal("expected exactly one business call")
			}
			legacy := request()
			legacy.Header.Set("Dapr-Caller-App-Id", tt.caller)
			want := serve(New(svc, nil, []string{"account-api", "engagement-api"}, false, cleaner), legacy)
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Fatal("private route changed the legacy response envelope")
			}
		})
	}
}
