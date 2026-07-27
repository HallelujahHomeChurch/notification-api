package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
)

type fakeService struct {
	sendCalls int
	getCalls  int
	send      func(context.Context, string, string, contracts.SendRequest) (service.Result, error)
	get       func(context.Context, string, string) (service.Result, error)
}

func (f *fakeService) Send(ctx context.Context, caller, key string, request contracts.SendRequest) (service.Result, error) {
	f.sendCalls++
	if f.send != nil {
		return f.send(ctx, caller, key, request)
	}
	return service.Result{MessageID: "message-1", Status: contracts.MessageStatusQueued, TemplateVersion: 1}, nil
}

func (f *fakeService) Get(ctx context.Context, caller, messageID string) (service.Result, error) {
	f.getCalls++
	if f.get != nil {
		return f.get(ctx, caller, messageID)
	}
	return service.Result{}, service.ErrNotFound
}

type fakePinger struct {
	calls int
	err   error
}

func (p *fakePinger) PingContext(context.Context) error {
	p.calls++
	return p.err
}

func TestSendRequiresDaprCaller(t *testing.T) {
	svc := &fakeService{}
	handler := New(svc, &fakePinger{}, []string{"account-api"}, false)
	response := serve(handler, sendRequest(validBody()))

	assertError(t, response, http.StatusUnauthorized, "NTF_UNAUTHORIZED")
	if svc.sendCalls != 0 {
		t.Fatalf("Send calls = %d, want 0", svc.sendCalls)
	}
}

func TestSendRejectsUnknownCaller(t *testing.T) {
	handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, false)
	request := sendRequest(validBody())
	request.Header.Set("Dapr-Caller-App-Id", "unknown-api")

	assertError(t, serve(handler, request), http.StatusForbidden, "NTF_FORBIDDEN")
}

func TestDevelopmentCallerHeaderRequiresExplicitOptIn(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		status  int
	}{
		{name: "disabled", status: http.StatusUnauthorized},
		{name: "enabled", enabled: true, status: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, test.enabled)
			request := sendRequest(validBody())
			request.Header.Set("X-HHC-Caller-App-Id", "account-api")
			request.Header.Set("Idempotency-Key", "operation-1")

			response := serve(handler, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body)
			}
		})
	}
}

func TestDaprCallerTakesPrecedenceOverDevelopmentHeader(t *testing.T) {
	handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, true)
	request := sendRequest(validBody())
	request.Header.Set("Dapr-Caller-App-Id", "unknown-api")
	request.Header.Set("X-HHC-Caller-App-Id", "account-api")
	request.Header.Set("Idempotency-Key", "operation-1")

	assertError(t, serve(handler, request), http.StatusForbidden, "NTF_FORBIDDEN")
}

func TestSendRequiresIdempotencyKey(t *testing.T) {
	svc := &fakeService{}
	handler := New(svc, &fakePinger{}, []string{"account-api"}, false)
	request := sendRequest(validBody())
	request.Header.Set("Dapr-Caller-App-Id", "account-api")

	assertError(t, serve(handler, request), http.StatusBadRequest, "NTF_INVALID_REQUEST")
	if svc.sendCalls != 0 {
		t.Fatalf("Send calls = %d, want 0", svc.sendCalls)
	}
}

func TestSendReturnsAcceptedEnvelopeAndRequestID(t *testing.T) {
	handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, false)
	request := authorizedSend(validBody())
	request.Header.Set("X-HHC-Request-ID", "request-123")

	response := serve(handler, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusAccepted, response.Body)
	}
	if got := response.Header().Get("X-HHC-Request-ID"); got != "request-123" {
		t.Fatalf("request ID header = %q", got)
	}
	envelope := decodeEnvelope(t, response)
	if envelope.Meta.RequestID != "request-123" || envelope.Error != nil {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Data["messageId"] != "message-1" || envelope.Data["status"] != "queued" {
		t.Fatalf("data = %#v", envelope.Data)
	}
}

func TestSendRejectsNonStrictJSON(t *testing.T) {
	oversized := strings.Replace(validBody(), "opaque", strings.Repeat("x", 64<<10), 1)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "unknown field", body: strings.TrimSuffix(validBody(), "}") + `,"unknown":true}`},
		{name: "trailing JSON", body: validBody() + `{}`},
		{name: "over 64KiB", body: oversized},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, false)
			assertError(t, serve(handler, authorizedSend(test.body)), http.StatusBadRequest, "NTF_INVALID_REQUEST")
		})
	}
}

func TestSendMapsServiceErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{name: "invalid", err: service.ErrInvalidRequest, status: http.StatusBadRequest, code: "NTF_INVALID_REQUEST"},
		{name: "forbidden template", err: service.ErrForbiddenTemplate, status: http.StatusForbidden, code: "NTF_FORBIDDEN"},
		{name: "idempotency conflict", err: service.ErrIdempotencyConflict, status: http.StatusConflict, code: "NTF_IDEMPOTENCY_CONFLICT"},
		{name: "rate limited without duration", err: service.ErrRateLimited, status: http.StatusTooManyRequests, code: "NTF_RATE_LIMITED", retryAfter: "1"},
		{name: "rate limited rounds up", err: &service.RateLimitError{RetryAfter: 1500 * time.Millisecond}, status: http.StatusTooManyRequests, code: "NTF_RATE_LIMITED", retryAfter: "2"},
		{name: "disabled", err: service.ErrNotificationsDisabled, status: http.StatusServiceUnavailable, code: "NTF_DISABLED"},
		{name: "internal", err: errors.New("database unavailable"), status: http.StatusInternalServerError, code: "NTF_INTERNAL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &fakeService{send: func(context.Context, string, string, contracts.SendRequest) (service.Result, error) {
				return service.Result{}, test.err
			}}
			response := serve(New(svc, &fakePinger{}, []string{"account-api"}, false), authorizedSend(validBody()))
			assertError(t, response, test.status, test.code)
			if got := response.Header().Get("Retry-After"); got != test.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, test.retryAfter)
			}
		})
	}
}

func TestStatusIsScopedToCaller(t *testing.T) {
	svc := &fakeService{get: func(_ context.Context, caller, messageID string) (service.Result, error) {
		if caller != "account-api" || messageID != "message-1" {
			return service.Result{}, service.ErrNotFound
		}
		return service.Result{MessageID: messageID, Status: contracts.MessageStatusSent, TemplateVersion: 1}, nil
	}}
	handler := New(svc, &fakePinger{}, []string{"account-api", "hhc-web-api"}, false)

	current := httptest.NewRequest(http.MethodGet, "/priv/notifications/message-1", nil)
	current.Header.Set("Dapr-Caller-App-Id", "account-api")
	response := serve(handler, current)
	if response.Code != http.StatusOK {
		t.Fatalf("same caller status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body)
	}
	envelope := decodeEnvelope(t, response)
	if envelope.Data["messageId"] != "message-1" || envelope.Data["status"] != "sent" {
		t.Fatalf("data = %#v", envelope.Data)
	}

	other := httptest.NewRequest(http.MethodGet, "/priv/notifications/message-1", nil)
	other.Header.Set("Dapr-Caller-App-Id", "hhc-web-api")
	assertError(t, serve(handler, other), http.StatusNotFound, "NTF_NOT_FOUND")
}

func TestHealthDoesNotCheckDependencies(t *testing.T) {
	db := &fakePinger{err: errors.New("database unavailable")}
	response := serve(New(&fakeService{}, db, nil, false), httptest.NewRequest(http.MethodGet, "/health", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if db.calls != 0 {
		t.Fatalf("PingContext calls = %d, want 0", db.calls)
	}
}

func TestReadyChecksDatabase(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "ready", status: http.StatusOK},
		{name: "database unavailable", err: errors.New("database unavailable"), status: http.StatusServiceUnavailable, code: "NTF_NOT_READY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := &fakePinger{err: test.err}
			response := serve(New(&fakeService{}, db, nil, false), httptest.NewRequest(http.MethodGet, "/ready", nil))
			if test.code == "" {
				if response.Code != test.status {
					t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body)
				}
			} else {
				assertError(t, response, test.status, test.code)
			}
			if db.calls != 1 {
				t.Fatalf("PingContext calls = %d, want 1", db.calls)
			}
		})
	}
}

func TestGetSendRouteNeverPerformsStatusLookup(t *testing.T) {
	svc := &fakeService{}
	handler := New(svc, &fakePinger{}, []string{"account-api"}, false)
	request := httptest.NewRequest(http.MethodGet, "/priv/notifications/send", nil)
	request.Header.Set("Dapr-Caller-App-Id", "account-api")

	assertError(t, serve(handler, request), http.StatusMethodNotAllowed, "NTF_METHOD_NOT_ALLOWED")
	if svc.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0", svc.getCalls)
	}
}

func TestKnownRoutesRejectEveryUnsupportedMethodWithEnvelope(t *testing.T) {
	routes := []struct {
		path   string
		method string
	}{
		{path: "/health", method: http.MethodGet},
		{path: "/ready", method: http.MethodGet},
		{path: "/priv/notifications/send", method: http.MethodPost},
		{path: "/priv/notifications/message-1", method: http.MethodGet},
	}
	methods := []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
	}
	handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, false)

	for _, route := range routes {
		for _, method := range methods {
			if method == route.method {
				continue
			}
			t.Run(method+" "+route.path, func(t *testing.T) {
				request := httptest.NewRequest(method, route.path, nil)
				assertError(t, serve(handler, request), http.StatusMethodNotAllowed, "NTF_METHOD_NOT_ALLOWED")
			})
		}
	}
}

func TestUnknownRouteReturnsNotFoundEnvelope(t *testing.T) {
	handler := New(&fakeService{}, &fakePinger{}, []string{"account-api"}, false)
	request := httptest.NewRequest(http.MethodGet, "/priv/unknown", nil)

	assertError(t, serve(handler, request), http.StatusNotFound, "NTF_NOT_FOUND")
}

func validBody() string {
	return `{"templateId":"account.verify-email","channel":"email","target":{"type":"email","address":"user@example.com"},"locale":"zh-Hant","payload":{"verifyUrl":"https://account.alive.org.tw/verify-email?token=opaque"},"resource":{"type":"account","id":"user-1"}}`
}

func sendRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/priv/notifications/send", bytes.NewBufferString(body))
}

func authorizedSend(body string) *http.Request {
	request := sendRequest(body)
	request.Header.Set("Dapr-Caller-App-Id", "account-api")
	request.Header.Set("Idempotency-Key", "operation-1")
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type envelope struct {
	Data  map[string]any           `json:"data"`
	Meta  contracts.ResponseMeta   `json:"meta"`
	Error *contracts.ResponseError `json:"error"`
}

func decodeEnvelope(t *testing.T, response *httptest.ResponseRecorder) envelope {
	t.Helper()
	var value envelope
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, response.Body)
	}
	return value
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body)
	}
	envelope := decodeEnvelope(t, response)
	if envelope.Data != nil || envelope.Error == nil || envelope.Error.Code != code || envelope.Meta.RequestID == "" {
		t.Fatalf("envelope = %+v, want code %s", envelope, code)
	}
}
