package notify

import (
	"context"
	"errors"
	"testing"
)

type fakeLimiter struct {
	err error
}

func (l fakeLimiter) Allow(context.Context, Message) error {
	return l.err
}

type fakeQueue struct {
	message Message
	calls   int
}

func (q *fakeQueue) Enqueue(_ context.Context, message Message) error {
	q.message = message
	q.calls++
	return nil
}

func TestServiceEnqueueBuildsVerificationJob(t *testing.T) {
	queue := &fakeQueue{}
	service := NewService(fakeLimiter{}, queue)

	err := service.EnqueueEmail(context.Background(), SendEmailRequest{
		Template: TemplateEmailVerification,
		To:       "USER@Example.COM",
		Data: map[string]string{
			"verify_url": "https://account.alive.org.tw/verify-email?token=abc",
		},
	})

	if err != nil {
		t.Fatalf("EnqueueEmail() error = %v", err)
	}
	if queue.calls != 1 {
		t.Fatalf("queue calls = %d, want 1", queue.calls)
	}
	if queue.message.To != "user@example.com" {
		t.Fatalf("To = %q, want normalized email", queue.message.To)
	}
	if queue.message.Template != TemplateEmailVerification {
		t.Fatalf("Template = %q", queue.message.Template)
	}
	if queue.message.Data["verify_url"] == "" {
		t.Fatalf("verify_url missing from queued message")
	}
}

func TestServiceEnqueueRejectsMissingTemplateData(t *testing.T) {
	service := NewService(fakeLimiter{}, &fakeQueue{})

	err := service.EnqueueEmail(context.Background(), SendEmailRequest{
		Template: TemplatePasswordReset,
		To:       "user@example.com",
		Data:     map[string]string{},
	})

	if err == nil {
		t.Fatal("EnqueueEmail() error = nil, want missing reset_url error")
	}
}

func TestServiceEnqueueDoesNotQueueWhenRateLimited(t *testing.T) {
	queue := &fakeQueue{}
	service := NewService(fakeLimiter{err: ErrRateLimited}, queue)

	err := service.EnqueueEmail(context.Background(), SendEmailRequest{
		Template: TemplateEmailVerification,
		To:       "user@example.com",
		Data: map[string]string{
			"verify_url": "https://account.alive.org.tw/verify-email?token=abc",
		},
	})

	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("EnqueueEmail() error = %v, want ErrRateLimited", err)
	}
	if queue.calls != 0 {
		t.Fatalf("queue calls = %d, want 0", queue.calls)
	}
}
