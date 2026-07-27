package notify

import (
	"context"
	"fmt"
	"log"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
)

type Consumer interface {
	Consume(context.Context, func(context.Context, Message) error) error
}

func RunWorker(ctx context.Context, consumer Consumer, sender Sender, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	return consumer.Consume(ctx, func(ctx context.Context, message Message) error {
		email, err := BuildEmail(message)
		if err != nil {
			logger.Printf("notification build failed template=%s to=%s error=%v", message.Template, message.To, err)
			return err
		}
		if err := sender.Send(ctx, email); err != nil {
			logger.Printf("notification send failed template=%s to=%s error=%v", message.Template, message.To, err)
			return err
		}
		logger.Printf("notification sent template=%s to=%s", message.Template, message.To)
		return nil
	})
}

// BuildEmail preserves the legacy worker contract until Task 9 replaces this runtime.
func BuildEmail(message Message) (Email, error) {
	templateID, payload, err := legacyTemplate(message)
	if err != nil {
		return Email{}, err
	}
	definition, err := templates.Resolve(templateID, "email")
	if err != nil {
		return Email{}, err
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
		return Email{}, err
	}
	rendered, err := templates.RenderEmail(definition, message.Locale, message.To, validated)
	if err != nil {
		return Email{}, err
	}
	return Email{To: rendered.To, Subject: rendered.Subject, Body: rendered.Body}, nil
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
