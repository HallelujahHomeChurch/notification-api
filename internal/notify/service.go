package notify

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

type Limiter interface {
	Allow(context.Context, Message) error
}

type Queue interface {
	Enqueue(context.Context, Message) error
}

type Service struct {
	limiter Limiter
	queue   Queue
}

func NewService(limiter Limiter, queue Queue) *Service {
	return &Service{limiter: limiter, queue: queue}
}

func (s *Service) EnqueueEmail(ctx context.Context, req SendEmailRequest) error {
	to, err := normalizeEmail(req.To)
	if err != nil {
		return err
	}

	message := Message{
		Template:  req.Template,
		To:        to,
		Locale:    req.Locale,
		Data:      req.Data,
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
