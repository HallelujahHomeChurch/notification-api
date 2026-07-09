package notify

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type handlerService struct {
	calls int
	err   error
}

func (s *handlerService) EnqueueEmail(context.Context, SendEmailRequest) error {
	s.calls++
	return s.err
}

func TestHandlerRejectsInvalidInternalToken(t *testing.T) {
	service := &handlerService{}
	handler := NewHandler(service, "secret")
	req := httptest.NewRequest(http.MethodPost, "/priv/notification/v1/email", bytes.NewBufferString(`{"template":"email_verification","to":"user@example.com","data":{"verify_url":"https://account.alive.org.tw"}}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if service.calls != 0 {
		t.Fatalf("service calls = %d, want 0", service.calls)
	}
}

func TestHandlerAcceptsValidInternalToken(t *testing.T) {
	service := &handlerService{}
	handler := NewHandler(service, "secret")
	req := httptest.NewRequest(http.MethodPost, "/priv/notification/v1/email", bytes.NewBufferString(`{"template":"email_verification","to":"user@example.com","data":{"verify_url":"https://account.alive.org.tw"}}`))
	req.Header.Set("X-Internal-Token", "secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if service.calls != 1 {
		t.Fatalf("service calls = %d, want 1", service.calls)
	}
}

func TestHandlerReturnsBadGatewayForQueueFailure(t *testing.T) {
	service := &handlerService{err: errors.New("queue down")}
	handler := NewHandler(service, "")
	req := httptest.NewRequest(http.MethodPost, "/priv/notification/v1/email", bytes.NewBufferString(`{"template":"email_verification","to":"user@example.com","data":{"verify_url":"https://account.alive.org.tw"}}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}
