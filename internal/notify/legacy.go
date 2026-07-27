package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
)

// Task 9 removes this compatibility surface with the legacy runtime.
const (
	TemplateEmailVerification = "email_verification"
	TemplatePasswordReset     = "password_reset"
)

var (
	ErrInvalidRequest = errors.New("invalid request")
	ErrRateLimited    = errors.New("rate limited")
	ErrDisabled       = errors.New("notification sending disabled")
)

type SendEmailRequest struct {
	Template string            `json:"template"`
	To       string            `json:"to"`
	Locale   string            `json:"locale,omitempty"`
	Data     map[string]string `json:"data"`
}

type Message struct {
	Template  string            `json:"template"`
	To        string            `json:"to"`
	Locale    string            `json:"locale,omitempty"`
	Data      map[string]string `json:"data"`
	CreatedAt time.Time         `json:"created_at"`
}

type Email struct {
	To      string
	Subject string
	Body    string
}

type Limiter interface {
	Allow(context.Context, Message) error
}

type Queue interface {
	Enqueue(context.Context, Message) error
}

type Consumer interface {
	Consume(context.Context, func(context.Context, Message) error) error
}

type Service struct {
	limiter Limiter
	queue   Queue
}

func NewService(limiter Limiter, queue Queue) *Service {
	return &Service{limiter: limiter, queue: queue}
}

func (s *Service) EnqueueEmail(ctx context.Context, request SendEmailRequest) error {
	to, err := normalizeEmail(request.To)
	if err != nil {
		return err
	}
	message := Message{
		Template:  request.Template,
		To:        to,
		Locale:    request.Locale,
		Data:      request.Data,
		CreatedAt: time.Now().UTC(),
	}
	if err := validateMessage(message); err != nil {
		return err
	}
	if err := s.limiter.Allow(ctx, message); err != nil {
		return err
	}
	return s.queue.Enqueue(ctx, message)
}

func normalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		return "", fmt.Errorf("%w: invalid email", ErrInvalidRequest)
	}
	return email, nil
}

func validateMessage(message Message) error {
	switch message.Template {
	case TemplateEmailVerification:
		if message.Data["verify_url"] == "" {
			return fmt.Errorf("%w: verify_url is required", ErrInvalidRequest)
		}
	case TemplatePasswordReset:
		if message.Data["reset_url"] == "" {
			return fmt.Errorf("%w: reset_url is required", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unsupported template", ErrInvalidRequest)
	}
	return nil
}

type MemoryQueue struct {
	ch chan Message
}

func NewMemoryQueue(size int) *MemoryQueue {
	if size <= 0 {
		size = 100
	}
	return &MemoryQueue{ch: make(chan Message, size)}
}

func (q *MemoryQueue) Enqueue(ctx context.Context, message Message) error {
	select {
	case q.ch <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *MemoryQueue) Consume(ctx context.Context, handle func(context.Context, Message) error) error {
	for {
		select {
		case message := <-q.ch:
			_ = handle(ctx, message)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func RunWorker(ctx context.Context, consumer Consumer, provider providers.Provider, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	return consumer.Consume(ctx, func(ctx context.Context, message Message) error {
		email, err := BuildEmail(message)
		if err != nil {
			logger.Printf("notification build failed template=%s", message.Template)
			return err
		}
		if _, err := provider.Send(ctx, email); err != nil {
			logger.Printf("notification send failed template=%s", message.Template)
			return err
		}
		logger.Printf("notification sent template=%s", message.Template)
		return nil
	})
}

// BuildEmail preserves the legacy worker contract until Task 9 replaces this runtime.
func BuildEmail(message Message) (providers.DeliveryPayload, error) {
	templateID, payload, err := legacyTemplate(message)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	definition, err := templates.Resolve(templateID, "email")
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	request := contracts.SendRequest{
		TemplateID: templateID,
		Channel:    "email",
		Target:     contracts.Target{Type: "email", Address: message.To},
		Locale:     message.Locale,
		Payload:    payload,
	}
	validated, err := templates.Validate(definition, "account-api", request)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	rendered, err := templates.RenderEmail(definition, message.Locale, message.To, validated)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	return providers.DeliveryPayload{
		Recipient: rendered.To,
		Subject:   rendered.Subject,
		Body:      rendered.Body,
	}, nil
}

func legacyTemplate(message Message) (string, map[string]string, error) {
	switch message.Template {
	case TemplateEmailVerification:
		return "account.verify-email", map[string]string{"verifyUrl": message.Data["verify_url"]}, nil
	case TemplatePasswordReset:
		return "account.reset-password", map[string]string{"resetUrl": message.Data["reset_url"]}, nil
	default:
		return "", nil, fmt.Errorf("unsupported template")
	}
}
