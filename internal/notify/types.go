package notify

import (
	"errors"
	"time"
)

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
