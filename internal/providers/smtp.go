package providers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

const defaultSMTPTimeout = 15 * time.Second

type SMTPConfig struct {
	Addr      string
	Username  string
	Password  string
	From      string
	FromName  string
	Timeout   time.Duration
	TLSConfig *tls.Config
	Logger    *log.Logger
}

type SMTP struct {
	config       SMTPConfig
	writeMessage func(io.Writer, string, string, DeliveryPayload) error
}

func NewSMTP(config SMTPConfig) *SMTP {
	if config.Timeout <= 0 {
		config.Timeout = defaultSMTPTimeout
	}
	return &SMTP{config: config, writeMessage: writeMessage}
}

func ValidateSMTPConfig(config SMTPConfig) error {
	_, err := smtpHost(config)
	return err
}

func (s *SMTP) Send(ctx context.Context, payload DeliveryPayload) (ProviderReceipt, error) {
	host, err := s.validate(payload)
	if err != nil {
		return ProviderReceipt{}, s.failed(ErrorInvalidEndpoint, "validate", err)
	}
	if err := ctx.Err(); err != nil {
		return ProviderReceipt{}, s.failed(ErrorTemporary, "dial", err)
	}

	conn, err := (&net.Dialer{Timeout: s.config.Timeout}).DialContext(ctx, "tcp", s.config.Addr)
	if err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "dial", contextCause(ctx, err))
	}
	defer conn.Close()

	deadline := time.Now().Add(s.config.Timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return ProviderReceipt{}, s.failed(ErrorTemporary, "deadline", err)
	}
	stopCancellation := interruptOnCancellation(ctx, conn)
	defer stopCancellation()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "greeting", contextCause(ctx, err))
	}
	defer client.Close()

	hasSTARTTLS, _ := client.Extension("STARTTLS")
	if !hasSTARTTLS {
		return ProviderReceipt{}, s.failed(ErrorPermanent, "starttls", errors.New("SMTP server does not support STARTTLS"))
	}
	tlsConfig := cloneTLSConfig(s.config.TLSConfig, host)
	if err := client.StartTLS(tlsConfig); err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "starttls", contextCause(ctx, err))
	}

	if s.config.Username != "" {
		hasAUTH, mechanisms := client.Extension("AUTH")
		if !hasAUTH {
			return ProviderReceipt{}, s.failed(ErrorPermanent, "auth", errors.New("SMTP server does not support authentication"))
		}
		var auth smtp.Auth
		switch {
		case containsWord(mechanisms, "PLAIN"):
			auth = smtp.PlainAuth("", s.config.Username, s.config.Password, host)
		case containsWord(mechanisms, "LOGIN"):
			auth = &loginAuth{username: s.config.Username, password: s.config.Password, host: host}
		default:
			return ProviderReceipt{}, s.failed(ErrorPermanent, "auth", errors.New("SMTP server does not support a configured authentication mechanism"))
		}
		if err := client.Auth(auth); err != nil {
			return ProviderReceipt{}, s.failed(classify(ctx, err), "auth", contextCause(ctx, err))
		}
	}
	if err := client.Mail(s.config.From); err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "mail", contextCause(ctx, err))
	}
	if err := client.Rcpt(payload.Recipient); err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "recipient", contextCause(ctx, err))
	}
	writer, err := client.Data()
	if err != nil {
		return ProviderReceipt{}, s.failed(classify(ctx, err), "data", contextCause(ctx, err))
	}
	if err := s.writeMessage(writer, s.config.From, s.config.FromName, payload); err != nil {
		_ = client.Close()
		return ProviderReceipt{}, s.failed(classify(ctx, err), "message", contextCause(ctx, err))
	}
	if err := writer.Close(); err != nil {
		return ProviderReceipt{}, s.failed(ErrorAcceptanceUnknown, "data", contextCause(ctx, err))
	}

	_ = client.Quit()
	receipt := ProviderReceipt{
		Provider:          "smtp",
		ProviderMessageID: payload.MessageID,
		AcceptedAt:        time.Now().UTC(),
	}
	if s.config.Logger != nil {
		s.config.Logger.Print("event=notification_provider_success provider=smtp")
	}
	return receipt, nil
}

func (s *SMTP) validate(payload DeliveryPayload) (string, error) {
	host, err := smtpHost(s.config)
	if err != nil {
		return "", err
	}
	if !plainAddress(payload.Recipient) {
		return "", errors.New("invalid email address")
	}
	if strings.ContainsAny(payload.Subject, "\r\n") {
		return "", errors.New("invalid subject")
	}
	if strings.ContainsAny(payload.ListUnsubscribe, "\r\n") {
		return "", errors.New("invalid unsubscribe header")
	}
	if !validMessageID(payload.MessageID) {
		return "", errors.New("invalid message ID")
	}
	return host, nil
}

func smtpHost(config SMTPConfig) (string, error) {
	host, port, err := net.SplitHostPort(config.Addr)
	if err != nil || host == "" || port == "" {
		return "", errors.New("invalid SMTP address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("invalid SMTP port")
	}
	if !plainAddress(config.From) {
		return "", errors.New("invalid email address")
	}
	if strings.ContainsAny(config.FromName, "\r\n") {
		return "", errors.New("invalid sender display name")
	}
	if (config.Username == "") != (config.Password == "") {
		return "", errors.New("SMTP credentials must be provided together")
	}
	return strings.Trim(host, "[]"), nil
}

func (s *SMTP) failed(kind ErrorKind, operation string, cause error) error {
	providerErr := &ProviderError{Kind: kind, Operation: operation, cause: cause}
	if s.config.Logger != nil {
		s.config.Logger.Printf(
			"event=notification_provider_failure provider=smtp kind=%s operation=%s",
			kind,
			operation,
		)
	}
	return providerErr
}

type loginAuth struct {
	username string
	password string
	host     string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	default:
		return nil, errors.New("unexpected SMTP authentication challenge")
	}
}

func classify(ctx context.Context, err error) ErrorKind {
	if ctx.Err() != nil {
		return ErrorTemporary
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		if smtpErr.Code >= 400 && smtpErr.Code < 500 {
			return ErrorTemporary
		}
		if smtpErr.Code >= 500 && smtpErr.Code < 600 {
			return ErrorPermanent
		}
	}
	return ErrorTemporary
}

func contextCause(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}

func interruptOnCancellation(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()
	return func() { close(done) }
}

func cloneTLSConfig(config *tls.Config, host string) *tls.Config {
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	return config
}

func plainAddress(value string) bool {
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value
}

func validMessageID(value string) bool {
	return len(value) >= 3 &&
		len(value) <= 998 &&
		strings.HasPrefix(value, "<") &&
		strings.HasSuffix(value, ">") &&
		!strings.ContainsAny(value, "\r\n \t") &&
		plainAddress(value[1:len(value)-1])
}

func containsWord(value, word string) bool {
	for _, candidate := range strings.Fields(value) {
		if strings.EqualFold(candidate, word) {
			return true
		}
	}
	return false
}

func writeMessage(writer io.Writer, from, fromName string, payload DeliveryPayload) error {
	fromHeader := from
	if fromName != "" {
		fromHeader = (&mail.Address{Name: fromName, Address: from}).String()
	}
	if payload.HTMLBody != "" {
		return writeMultipartMessage(writer, fromHeader, payload)
	}
	headers := strings.Join(messageHeaders(fromHeader, payload, "Content-Type: text/plain; charset=UTF-8", "Content-Transfer-Encoding: quoted-printable"), "\r\n")
	if _, err := io.WriteString(writer, headers+"\r\n"); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}
	body := quotedprintable.NewWriter(writer)
	if _, err := io.WriteString(body, payload.Body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := body.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return nil
}

func writeMultipartMessage(writer io.Writer, from string, payload DeliveryPayload) error {
	multipartWriter := multipart.NewWriter(writer)
	headers := strings.Join(messageHeaders(from, payload, "Content-Type: multipart/alternative; boundary="+strconv.Quote(multipartWriter.Boundary())), "\r\n")
	if _, err := io.WriteString(writer, headers+"\r\n"); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}
	for _, part := range []struct {
		contentType string
		body        string
	}{
		{"text/plain; charset=UTF-8", payload.Body},
		{"text/html; charset=UTF-8", payload.HTMLBody},
	} {
		partWriter, err := multipartWriter.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return fmt.Errorf("create MIME part: %w", err)
		}
		bodyWriter := quotedprintable.NewWriter(partWriter)
		if _, err := io.WriteString(bodyWriter, part.body); err != nil {
			return fmt.Errorf("write MIME part: %w", err)
		}
		if err := bodyWriter.Close(); err != nil {
			return fmt.Errorf("close MIME part: %w", err)
		}
	}
	if err := multipartWriter.Close(); err != nil {
		return fmt.Errorf("close MIME message: %w", err)
	}
	return nil
}

func messageHeaders(from string, payload DeliveryPayload, extra ...string) []string {
	headers := []string{"From: " + from, "To: " + payload.Recipient, "Subject: " + mime.QEncoding.Encode("UTF-8", payload.Subject), "Message-ID: " + payload.MessageID, "MIME-Version: 1.0"}
	if payload.ListUnsubscribe != "" {
		headers = append(headers, "List-Unsubscribe: "+payload.ListUnsubscribe)
		if payload.OneClickUnsubscribe {
			headers = append(headers, "List-Unsubscribe-Post: List-Unsubscribe=One-Click")
		}
	}
	return append(headers, append(extra, "")...)
}
