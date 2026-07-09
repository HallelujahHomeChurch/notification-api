package notify

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"strings"
)

type Sender interface {
	Send(context.Context, Email) error
}

type LogSender struct {
	Logger *log.Logger
}

func (s LogSender) Send(_ context.Context, email Email) error {
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("notification email accepted to=%s subject=%q", email.To, email.Subject)
	return nil
}

type SMTPSender struct {
	Addr     string
	Username string
	Password string
	From     string
}

func (s SMTPSender) Send(ctx context.Context, email Email) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Addr == "" || s.From == "" {
		return fmt.Errorf("smtp addr and from are required")
	}

	host := s.Addr
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	var auth smtp.Auth
	if s.Username != "" || s.Password != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, host)
	}

	body := strings.Join([]string{
		"From: " + s.From,
		"To: " + email.To,
		"Subject: " + email.Subject,
		"Content-Type: text/plain; charset=UTF-8",
		"",
		email.Body,
	}, "\r\n")
	return smtp.SendMail(s.Addr, auth, s.From, []string{email.To}, []byte(body))
}
